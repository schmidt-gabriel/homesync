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

    @Test("a rule saved elsewhere takes effect without a restart")
    func picksUpNewRulesMidRun() async throws {
        let machine = try TestMachine()

        // A file synced before anyone thought to exclude it, which is the only
        // shape this problem ever has.
        let marker = "ignored-\(UUID().uuidString.prefix(8)).txt"
        try machine.write(marker, "content")
        try await machine.engine.syncOnce()
        #expect(try await machine.server.read(machine.scoped(marker)) == "content")

        // Saved through a second connection, standing in for the admin UI or
        // another machine. The pattern names only this test's own file: the
        // document is shared by every test on this server, and saving it now
        // removes what it matches.
        let previous = try await machine.server.api.ignoreRules().rules
        try await machine.server.api.setIgnoreRules("# \(marker)\n\(marker)\n")
        defer { Task { try? await machine.server.api.setIgnoreRules(previous) } }

        // The engine has been running throughout, and used to hold the rules it
        // fetched at launch: it read the server clearing the path out as a real
        // deletion and removed the local copy — or, past the delete guard's
        // limit, stopped syncing altogether.
        try await machine.engine.syncOnce()

        #expect(machine.exists(marker), "an excluded file must stay on the machine that holds it")
        await #expect(throws: (any Error).self) {
            try await machine.server.read(machine.scoped(marker))
        }
        if case .paused(let reason) = await machine.engine.currentState {
            Issue.record("paused over a path the new rules exclude: \(reason)")
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

    @Test("losing the state database does not delete the folder")
    func forgottenStateKeepsLocalFiles() async throws {
        let machine = try TestMachine()

        for index in 1...10 {
            try machine.write("kept-\(index).txt", "content \(index)")
        }
        try await machine.engine.syncOnce()

        // The server no longer has them: deleted from another machine, or
        // dropped when an ignore rule started matching them. Either way the
        // change set is now ten tombstones for paths that exist here.
        for index in 1...10 {
            try await machine.server.delete(machine.scoped("kept-\(index).txt"))
        }

        // The app restarts and cannot find its state database.
        let forgetful = try machine.withForgottenState()
        try await forgetful.engine.syncOnce()

        // Ten tombstones against an empty record used to be read as ten
        // deletions to apply, against a limit computed as a fraction of
        // nothing — so the engine paused, and would have deleted the lot had
        // the guard not been there. Neither is right: with no record of these
        // files, the server's word is not evidence that they should go.
        #expect(forgetful.files().count == 10)
        #expect(forgetful.read("kept-1.txt") == "content 1")

        if case .paused(let reason) = await forgetful.engine.currentState {
            Issue.record("the engine paused over deletions it should not make: \(reason)")
        }

        // They exist here and not there, so they go back up.
        #expect(try await forgetful.server.read(forgetful.scoped("kept-1.txt")) == "content 1")
    }

    @Test("ignored paths are not counted against the delete guard")
    func ignoredTombstonesDoNotTripTheGuard() async throws {
        let machine = try TestMachine(maxDeletes: 3)

        // Present on both sides, and ignored here. `*.swp` is platform noise,
        // so the rule needs no cooperation from the shared server — these
        // tests run against one instance, and a test that rewrote the ignore
        // document would change what every other test syncs.
        for index in 1...10 {
            try machine.write("session-\(index).swp", "editor scratch \(index)")
            try await machine.server.write(machine.scoped("session-\(index).swp"), "scratch")
        }
        for index in 1...10 {
            try await machine.server.delete(machine.scoped("session-\(index).swp"))
        }

        // The change set holds ten tombstones for paths that exist on this
        // disk, and the guard used to count all ten — although the ignore
        // rules drop every one of them before anything is applied, so the pull
        // was never going to delete a thing.
        try await machine.engine.syncOnce()

        // Asked of the disk directly: `files()` filters by the same rules, so
        // it would report these missing whether or not they survived.
        #expect((1...10).allSatisfy { machine.exists("session-\($0).swp") })
        if case .paused(let reason) = await machine.engine.currentState {
            Issue.record("paused over ignored paths: \(reason)")
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

    @Test("a file rewritten during the sync never lands corrupted")
    func fileRewrittenMidSyncIsNotCorrupted() async throws {
        let machine = try TestMachine()

        // Big enough that the upload cannot finish in one read, which is what
        // gives the rewrite a window to land in the middle of it.
        let original = String(repeating: "A", count: 4_000_000)
        try machine.write("document.md", original)

        // An editor saving over the file while it is being read. Before the
        // snapshot, this produced a body padded to the old length: URLSession
        // fixes the content length when the request is made, so a file that
        // shrinks underneath it is sent with trailing NULs.
        let rewriter = Task {
            try? await Task.sleep(for: .milliseconds(15))
            try? machine.write("document.md", "rewritten, much shorter")
        }

        _ = try? await machine.engine.syncOnce()
        await rewriter.value

        // Whichever version won, the server must hold that version exactly.
        // A body that belongs to neither is the failure being guarded against.
        try await machine.engine.syncOnce()
        let stored = try await machine.server.read(machine.scoped("document.md"))

        #expect(stored == original || stored == "rewritten, much shorter")
        #expect(!stored.contains("\0"))
        #expect(stored == machine.read("document.md"))
    }

    @Test("the server refuses a body that does not match its declared hash")
    func serverRejectsMismatchedHash() async throws {
        let machine = try TestMachine()

        let temporary = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "mismatch-\(UUID().uuidString)")
        try "the real content".write(to: temporary, atomically: true, encoding: .utf8)
        defer { try? FileManager.default.removeItem(at: temporary) }

        // The backstop: a client that computed its hash from different bytes
        // than it sent should be told, not quietly believed.
        await #expect(throws: (any Error).self) {
            try await machine.server.api.upload(
                path: machine.scoped("mismatch.txt"), from: temporary, baseRev: 0,
                sha256: String(repeating: "0", count: 64))
        }

        await #expect(throws: (any Error).self) {
            try await machine.server.read(machine.scoped("mismatch.txt"))
        }
    }
}
