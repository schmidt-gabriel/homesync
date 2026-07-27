import Foundation
import Testing

@testable import HomeSyncKit

@Suite("Local state")
struct LocalStateTests {
    private func temporaryDatabase() -> URL {
        URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "homesynckit-tests-\(UUID().uuidString)")
            .appending(path: "state.sqlite")
    }

    @Test("records and reads back an entry")
    func roundTrip() throws {
        let state = try LocalState(at: temporaryDatabase())

        let entry = SyncedState(
            path: "projects/alpha/notes.md", type: .file,
            size: 1234, mtime: 1_785_194_027_083,
            sha256: "27eb5e51506c911f6fc4bb345c0d9db6f60415fceab7c18e1e9b862637415777", rev: 42)

        try state.record(entry)
        #expect(try state.state(for: entry.path) == entry)
    }

    @Test("recording the same path twice updates rather than duplicates")
    func upsert() throws {
        let state = try LocalState(at: temporaryDatabase())

        try state.record(SyncedState(
            path: "notes.md", type: .file, size: 10, mtime: 1, sha256: "aaa", rev: 1))
        try state.record(SyncedState(
            path: "notes.md", type: .file, size: 20, mtime: 2, sha256: "bbb", rev: 2))

        #expect(try state.count() == 1)
        #expect(try state.state(for: "notes.md")?.rev == 2)
        #expect(try state.state(for: "notes.md")?.sha256 == "bbb")
    }

    @Test("an unknown path reads as nil, not as a zero row")
    func missingPath() throws {
        let state = try LocalState(at: temporaryDatabase())
        #expect(try state.state(for: "never-seen.md") == nil)
    }

    @Test("forget removes an entry")
    func forget() throws {
        let state = try LocalState(at: temporaryDatabase())

        try state.record(SyncedState(
            path: "notes.md", type: .file, size: 1, mtime: 1, sha256: "a", rev: 1))
        try state.forget("notes.md")

        #expect(try state.state(for: "notes.md") == nil)
        #expect(try state.count() == 0)
    }

    @Test("the revision cursor survives reopening")
    func cursorPersists() throws {
        let url = temporaryDatabase()

        do {
            let state = try LocalState(at: url)
            state.lastRev = 87
            state.ignoreVersion = 1_785_194_027_083
            state.ignoreRules = "*.tmp\n"
        }

        // The cursor is the client's entire notion of where it is. Losing it
        // across a restart would mean re-downloading the world.
        let reopened = try LocalState(at: url)
        #expect(reopened.lastRev == 87)
        #expect(reopened.ignoreVersion == 1_785_194_027_083)
        #expect(reopened.ignoreRules == "*.tmp\n")
    }

    @Test("a fresh database starts at revision zero")
    func freshDatabase() throws {
        let state = try LocalState(at: temporaryDatabase())

        // Zero means "I have seen nothing", which makes the first sync a full
        // reconciliation rather than a no-op.
        #expect(state.lastRev == 0)
        #expect(try state.count() == 0)
    }

    @Test("allStates returns everything, keyed by path")
    func allStates() throws {
        let state = try LocalState(at: temporaryDatabase())

        for index in 1...5 {
            try state.record(SyncedState(
                path: "file-\(index).txt", type: .file,
                size: Int64(index), mtime: Int64(index), sha256: "sha\(index)", rev: Int64(index)))
        }

        let all = try state.allStates()
        #expect(all.count == 5)
        #expect(all["file-3.txt"]?.sha256 == "sha3")
    }

    @Test("a failed transaction leaves nothing behind")
    func transactionRollback() throws {
        let state = try LocalState(at: temporaryDatabase())

        struct Failure: Error {}

        // A half-applied batch would leave the state lying about what is on
        // disk, which is worse than not applying it at all.
        #expect(throws: Failure.self) {
            try state.transaction {
                try state.record(SyncedState(
                    path: "a.txt", type: .file, size: 1, mtime: 1, sha256: "a", rev: 1))
                throw Failure()
            }
        }

        #expect(try state.count() == 0)
    }

    @Test("paths with awkward characters survive")
    func awkwardPaths() throws {
        let state = try LocalState(at: temporaryDatabase())

        let paths = [
            "ação.txt",
            "quotes'and\"doubles.md",
            "a; DROP TABLE state; --.txt",
            "spaces everywhere/and more.md",
        ]

        for path in paths {
            try state.record(SyncedState(
                path: path, type: .file, size: 1, mtime: 1, sha256: "a", rev: 1))
        }

        #expect(try state.count() == paths.count)
        for path in paths {
            #expect(try state.state(for: path)?.path == path)
        }
    }
}
