import Foundation
import Testing

@testable import HomeSyncKit

@Suite("File store")
struct FileStoreTests {
    private func temporaryRoot() throws -> URL {
        let url = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "homesynckit-store-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true)
        return url
    }

    private func write(_ content: String, to store: FileStore, at path: String) throws {
        let url = store.url(for: path)
        try FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try content.write(to: url, atomically: true, encoding: .utf8)
    }

    @Test("relative paths are resolved against the real root")
    func relativePaths() throws {
        let store = try FileStore(root: temporaryRoot())

        #expect(store.relativePath(for: store.url(for: "notes.md")) == "notes.md")
        #expect(store.relativePath(for: store.url(for: "a/b/c.txt")) == "a/b/c.txt")

        // FSEvents reports paths through /private, while Foundation names the
        // same directory without it, because /var and /tmp are firmlinks that
        // neither realpath nor resolvingSymlinksInPath traverses. A path
        // arriving in the FSEvents spelling must still resolve, or the watcher
        // would silently observe nothing.
        let asFSEventsReportsIt = URL(fileURLWithPath: "/private" + store.url(for: "a/b/c.txt").path)
        #expect(store.relativePath(for: asFSEventsReportsIt) == "a/b/c.txt")
    }

    @Test("paths outside the root are rejected")
    func outsideRoot() throws {
        let store = try FileStore(root: temporaryRoot())

        #expect(store.relativePath(for: URL(fileURLWithPath: "/etc/passwd")) == nil)
        #expect(store.relativePath(for: store.root) == nil)
    }

    @Test("scanning finds files and directories, and honours the rules")
    func scanning() throws {
        let store = try FileStore(root: temporaryRoot())

        try write("a", to: store, at: "notes.md")
        try write("b", to: store, at: "projects/alpha/deep.txt")
        try write("junk", to: store, at: ".DS_Store")
        try write("more junk", to: store, at: "projects/.DS_Store")

        let found = try store.scan(ignoring: IgnoreRules(rules: ""))

        #expect(found["notes.md"]?.type == .file)
        #expect(found["projects"]?.type == .dir)
        #expect(found["projects/alpha/deep.txt"]?.type == .file)
        #expect(found[".DS_Store"] == nil)
        #expect(found["projects/.DS_Store"] == nil)
    }

    @Test("an ignored directory is not descended into")
    func scanSkipsIgnoredDirectories() throws {
        let store = try FileStore(root: temporaryRoot())

        try write("x", to: store, at: "build/output.o")
        try write("y", to: store, at: "build/deep/other.o")
        try write("z", to: store, at: "src/main.swift")

        let found = try store.scan(ignoring: IgnoreRules(rules: "build/"))

        #expect(found["src/main.swift"] != nil)
        #expect(found["build"] == nil)
        #expect(found["build/output.o"] == nil)
        #expect(found["build/deep/other.o"] == nil)
    }

    @Test("hashing matches the known SHA-256 of the content")
    func hashing() throws {
        let store = try FileStore(root: temporaryRoot())
        try write("hello world", to: store, at: "hello.txt")

        #expect(try store.hash("hello.txt")
            == "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9")
    }

    @Test("installing is atomic and carries the modification time over")
    func install() throws {
        let store = try FileStore(root: temporaryRoot())

        let temporary = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "incoming-\(UUID().uuidString)")
        try "downloaded".write(to: temporary, atomically: true, encoding: .utf8)

        let mtime: Int64 = 1_785_194_027_000
        try store.install(temporary, at: "a/b/installed.txt", modified: mtime)

        #expect(try String(contentsOf: store.url(for: "a/b/installed.txt"), encoding: .utf8)
            == "downloaded")

        // Carrying the server's mtime over is what stops the next scan seeing
        // a change and re-uploading what we just downloaded.
        let described = try #require(store.describe("a/b/installed.txt"))
        #expect(abs(described.mtime - mtime) < 1000)
    }

    @Test("installing over an existing file replaces it")
    func installReplaces() throws {
        let store = try FileStore(root: temporaryRoot())
        try write("old", to: store, at: "notes.md")

        let temporary = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "incoming-\(UUID().uuidString)")
        try "new".write(to: temporary, atomically: true, encoding: .utf8)

        try store.install(temporary, at: "notes.md", modified: nil)

        #expect(try String(contentsOf: store.url(for: "notes.md"), encoding: .utf8) == "new")
    }

    @Test("removing a path that is already gone is not an error")
    func removeIsIdempotent() throws {
        let store = try FileStore(root: temporaryRoot())
        try store.remove("never-existed.txt")
    }

    @Test("symlinks are skipped rather than followed")
    func symlinksSkipped() throws {
        let store = try FileStore(root: temporaryRoot())
        try write("real", to: store, at: "real.txt")

        try FileManager.default.createSymbolicLink(
            at: store.url(for: "link.txt"), withDestinationURL: store.url(for: "real.txt"))

        // Out of scope for v1, matching the server. Following them would mean
        // deciding what to do with targets outside the root.
        let found = try store.scan(ignoring: IgnoreRules(rules: ""))
        #expect(found["real.txt"] != nil)
        #expect(found["link.txt"] == nil)
        #expect(store.describe("link.txt") == nil)
    }

    @Test("decomposed filenames are reported composed")
    func normalisation() throws {
        let store = try FileStore(root: temporaryRoot())

        // Escapes, not literals: as plain text these would be at the mercy of
        // any tool that normalises this source file.
        let decomposed = "a\u{63}\u{327}a\u{303}o.txt"  // c + combining cedilla
        let composed = "a\u{e7}\u{e3}o.txt"  // precomposed ç, ã

        // Swift compares Strings by Unicode canonical equivalence, so these two
        // are `==` and hash alike. Convenient — a dictionary lookup finds the
        // entry whichever form you ask with — but it means the distinction only
        // exists in the bytes, which is exactly what goes on the wire.
        #expect(decomposed == composed)
        #expect(Array(decomposed.utf8) != Array(composed.utf8))

        try write("unicode", to: store, at: decomposed)

        // So the assertion that matters is at byte level: the key the scan
        // produced must be composed, because the server's index is.
        let found = try store.scan(ignoring: IgnoreRules(rules: ""))
        let key = try #require(found.keys.first { $0.hasSuffix("o.txt") })
        #expect(Array(key.utf8) == Array(composed.utf8))
    }
}
