import Foundation
import Testing

@testable import HomeSyncKit

/// Drives the real engine against a real server.
///
/// Unit tests cannot answer the question that actually matters — does a change
/// on one machine reach another — so these run against a live server and are
/// skipped without one:
///
///     docker compose up -d --build
///     TOKEN=$(docker compose exec -T homesync homesync device add mac | grep -oE '[A-Za-z0-9_-]{43}')
///     HOMESYNC_TEST_URL=http://localhost:8420 HOMESYNC_TEST_TOKEN=$TOKEN swift test
@Suite("Sync engine", .serialized, .enabled(if: TestServer.isConfigured))
struct SyncEngineTests {
    // MARK: - Local to server

    @Test("a new local file reaches the server")
    func uploadsNewFile() async throws {
        let machine = try TestMachine()
        try machine.write("notes.md", "written locally")

        let summary = try await machine.engine.syncOnce()

        #expect(summary.uploaded == 1)
        #expect(try await machine.server.read(machine.scoped("notes.md")) == "written locally")
    }

    @Test("a local edit reaches the server")
    func uploadsEdit() async throws {
        let machine = try TestMachine()
        try machine.write("notes.md", "first")
        try await machine.engine.syncOnce()

        try machine.write("notes.md", "second")
        let summary = try await machine.engine.syncOnce()

        #expect(summary.uploaded == 1)
        #expect(try await machine.server.read(machine.scoped("notes.md")) == "second")
    }

    @Test("an unchanged file is not uploaded again")
    func skipsUnchangedFiles() async throws {
        let machine = try TestMachine()
        try machine.write("notes.md", "stable")
        try await machine.engine.syncOnce()

        let second = try await machine.engine.syncOnce()

        // Re-uploading unchanged content would make every cycle cost as much
        // as the first one.
        #expect(second.uploaded == 0)
        #expect(second.deletedRemotely == 0)
    }

    @Test("touching a file without changing it does not upload")
    func skipsTouchedButIdenticalFiles() async throws {
        let machine = try TestMachine()
        try machine.write("notes.md", "stable")
        try await machine.engine.syncOnce()

        // Only the modification date moves. A stray `touch` must not wake
        // every other machine.
        try machine.touch("notes.md", at: Date().addingTimeInterval(60))

        #expect(try await machine.engine.syncOnce().uploaded == 0)
    }

    @Test("a local deletion reaches the server")
    func propagatesLocalDeletion() async throws {
        let machine = try TestMachine()
        try machine.write("doomed.txt", "temporary")
        try await machine.engine.syncOnce()

        try machine.remove("doomed.txt")
        let summary = try await machine.engine.syncOnce()

        #expect(summary.deletedRemotely == 1)
        await #expect(throws: (any Error).self) {
            try await machine.server.read(machine.scoped("doomed.txt"))
        }
    }

    @Test("nested directories are created on the way up")
    func uploadsNestedPaths() async throws {
        let machine = try TestMachine()
        try machine.write("a/b/c/deep.txt", "deep")

        try await machine.engine.syncOnce()

        #expect(try await machine.server.read(machine.scoped("a/b/c/deep.txt")) == "deep")
    }

    @Test("an ignored file never leaves the machine")
    func honoursIgnoreRules() async throws {
        let machine = try TestMachine()
        try machine.write(".DS_Store", "junk")
        try machine.write("real.txt", "content")

        try await machine.engine.syncOnce()

        #expect(try await machine.server.read(machine.scoped("real.txt")) == "content")
        await #expect(throws: (any Error).self) {
            try await machine.server.read(machine.scoped(".DS_Store"))
        }
    }

    // MARK: - Server to local

    @Test("a file created on the server arrives locally")
    func downloadsNewFile() async throws {
        let machine = try TestMachine()
        try await machine.server.write(machine.scoped("remote.txt"), "written remotely")

        let summary = try await machine.engine.syncOnce()

        #expect(summary.downloaded >= 1)
        #expect(machine.read("remote.txt") == "written remotely")
    }

    @Test("a deletion on the server removes the local file")
    func propagatesRemoteDeletion() async throws {
        let machine = try TestMachine()
        try machine.write("shared.txt", "content")
        try await machine.engine.syncOnce()

        try await machine.server.delete(machine.scoped("shared.txt"))
        let summary = try await machine.engine.syncOnce()

        #expect(summary.deletedLocally == 1)
        #expect(!machine.exists("shared.txt"))
    }

    @Test("a remote deletion does not discard an unsynced local edit")
    func remoteDeletionKeepsLocalEdit() async throws {
        let machine = try TestMachine()
        try machine.write("contested.txt", "original")
        try await machine.engine.syncOnce()

        // Deleted there, edited here, neither side aware of the other.
        try await machine.server.delete(machine.scoped("contested.txt"))
        try machine.write("contested.txt", "edited while it was being deleted")

        try await machine.engine.syncOnce()

        // Applying the deletion would silently throw away the edit. Keeping the
        // file and re-uploading it is the only answer that loses nothing.
        #expect(machine.read("contested.txt") == "edited while it was being deleted")
        #expect(try await machine.server.read(machine.scoped("contested.txt"))
            == "edited while it was being deleted")
    }

    // MARK: - Two machines

    @Test("a change on one machine reaches another")
    func propagatesBetweenMachines() async throws {
        let scope = TestMachine.newScope()
        let first = try TestMachine(scope: scope)
        let second = try TestMachine(scope: scope)

        try first.write("shared.md", "from the first machine")
        try await first.engine.syncOnce()
        try await second.engine.syncOnce()

        #expect(second.read("shared.md") == "from the first machine")
    }

    @Test("a deletion on one machine reaches another")
    func propagatesDeletionBetweenMachines() async throws {
        let scope = TestMachine.newScope()
        let first = try TestMachine(scope: scope)
        let second = try TestMachine(scope: scope)

        try first.write("shared.md", "content")
        try await first.engine.syncOnce()
        try await second.engine.syncOnce()
        #expect(second.exists("shared.md"))

        try first.remove("shared.md")
        try await first.engine.syncOnce()
        try await second.engine.syncOnce()

        #expect(!second.exists("shared.md"))
    }

    @Test("editing on both machines keeps both versions")
    func concurrentEditsKeepBothVersions() async throws {
        let scope = TestMachine.newScope()
        let first = try TestMachine(scope: scope, device: "first")
        let second = try TestMachine(scope: scope, device: "second")

        try first.write("contested.md", "the original")
        try await first.engine.syncOnce()
        try await second.engine.syncOnce()

        // Both edit from the same base, neither having seen the other.
        try first.write("contested.md", "the first machine's version")
        try second.write("contested.md", "the second machine's version")

        try await first.engine.syncOnce()
        try await second.engine.syncOnce()
        // A second pass brings the conflict copy back down to the first.
        try await first.engine.syncOnce()

        // Nothing may be lost. Both bodies must exist somewhere on both
        // machines, whichever of them ended up under the canonical name.
        for machine in [first, second] {
            let bodies = machine.allContents()
            #expect(bodies.contains("the first machine's version"))
            #expect(bodies.contains("the second machine's version"))
        }

        // Which machine records it depends on who pulls first; what matters
        // is that one of them noticed and can tell the user.
        let firstNoticed = await first.engine.conflicts
        let secondNoticed = await second.engine.conflicts
        #expect(!firstNoticed.isEmpty || !secondNoticed.isEmpty)
    }

    // MARK: - Safety

    @Test("a mass deletion is refused rather than applied")
    func refusesMassDeletion() async throws {
        let machine = try TestMachine(maxDeletes: 3)

        for index in 1...10 {
            try machine.write("file-\(index).txt", "content \(index)")
        }
        try await machine.engine.syncOnce()

        // Everything vanishes from the server, as it would if its volume had
        // failed to mount.
        for index in 1...10 {
            try await machine.server.delete(machine.scoped("file-\(index).txt"))
        }

        await #expect(throws: (any Error).self) {
            try await machine.engine.syncOnce()
        }

        // The guard has to actually protect the files, not just report.
        #expect(machine.files().count == 10)

        if case .paused(let reason) = await machine.engine.currentState {
            #expect(reason.contains("refusing to delete"))
        } else {
            Issue.record("expected the engine to pause, got \(await machine.engine.currentState)")
        }
    }

    @Test("a download is not re-uploaded on the next cycle")
    func downloadsDoNotEcho() async throws {
        let machine = try TestMachine()
        try await machine.server.write(machine.scoped("remote.txt"), "written remotely")

        try await machine.engine.syncOnce()
        let second = try await machine.engine.syncOnce()

        // A file we just wrote looks, to the scanner, exactly like one the user
        // edited. Without the state recording what we installed, the client
        // would push back what it just pulled, forever.
        #expect(second.uploaded == 0)
    }

    @Test("the engine resumes where it left off after a restart")
    func resumesAcrossRestart() async throws {
        let scope = TestMachine.newScope()
        let machine = try TestMachine(scope: scope)

        try machine.write("before.txt", "content")
        try await machine.engine.syncOnce()

        // Same root and same state database: a restart, not a fresh install.
        let restarted = try machine.restarted()
        let summary = try await restarted.engine.syncOnce()

        #expect(summary.uploaded == 0)
        #expect(summary.deletedRemotely == 0)
        #expect(restarted.read("before.txt") == "content")
    }
}
