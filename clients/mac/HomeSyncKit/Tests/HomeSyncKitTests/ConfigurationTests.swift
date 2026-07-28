import Foundation
import Testing

@testable import HomeSyncKit

@Suite("Configuration")
struct ConfigurationTests {
    /// The value is written out rather than compared against a second call in
    /// the same process, which is the whole point.
    ///
    /// `hashValue` is stable *within* one run and random *between* runs, so an
    /// `#expect(a == b)` over two calls passes happily while the app forgets
    /// everything it knows on every launch. Only a constant catches that.
    @Test("the state file for a root has one name, forever")
    func stateKeyIsStableAcrossProcesses() {
        let root = URL(fileURLWithPath: "/Users/example/HomeSync", isDirectory: true)

        // sha256("/Users/example/HomeSync"), first eight bytes.
        #expect(Configuration.stateKey(for: root) == "bb67a14c669d6b69")
    }

    @Test("two roots do not share a state file")
    func differentRootsDifferentKeys() {
        let one = URL(fileURLWithPath: "/Users/example/HomeSync", isDirectory: true)
        let other = URL(fileURLWithPath: "/Users/example/Other", isDirectory: true)

        #expect(Configuration.stateKey(for: one) != Configuration.stateKey(for: other))
    }

    /// A folder reached through a symlink is the same folder, and has to keep
    /// the same record. `FileStore` resolves its root, so a second key here
    /// would give the engine an empty state over a directory that is already
    /// full — the exact shape of the bug this guards.
    @Test("a symlink to a root shares the root's state file")
    func symlinkedRootSharesKey() throws {
        let base = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "homesynckit-config-\(UUID().uuidString)")
        let real = base.appending(path: "real")
        let link = base.appending(path: "link")

        try FileManager.default.createDirectory(at: real, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: base) }
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: real)

        #expect(Configuration.stateKey(for: link) == Configuration.stateKey(for: real))
    }
}
