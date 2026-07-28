import Foundation

@testable import HomeSyncKit

/// Where the live server is, if these tests were given one.
enum TestServer {
    static var url: URL? {
        guard let text = ProcessInfo.processInfo.environment["HOMESYNC_TEST_URL"] else { return nil }
        return URL(string: text)
    }

    static var token: String? {
        ProcessInfo.processInfo.environment["HOMESYNC_TEST_TOKEN"]
    }

    static var isConfigured: Bool { url != nil && token != nil }
}

struct TestServerUnavailable: Error {}

/// A direct line to the server, bypassing the engine.
///
/// The engine cannot be both the thing under test and the thing that confirms
/// it worked, so the assertions talk to the server themselves.
struct ServerProbe {
    let api: APIClient

    func write(_ path: String, _ content: String) async throws {
        let temporary = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "probe-\(UUID().uuidString)")
        try content.write(to: temporary, atomically: true, encoding: .utf8)
        defer { try? FileManager.default.removeItem(at: temporary) }

        let baseRev = (try? await currentRev(path)) ?? 0
        try await api.upload(path: path, from: temporary, baseRev: baseRev)
    }

    func read(_ path: String) async throws -> String {
        let download = try await api.download(path: path)
        defer { try? FileManager.default.removeItem(at: download.file) }
        return try String(contentsOf: download.file, encoding: .utf8)
    }

    func delete(_ path: String) async throws {
        try await api.delete(path: path, baseRev: try await currentRev(path))
    }

    private func currentRev(_ path: String) async throws -> Int64 {
        let download = try await api.download(path: path)
        try? FileManager.default.removeItem(at: download.file)
        return download.rev
    }
}

/// One simulated machine: its own folder, its own state database, its own
/// engine, all pointed at the shared server.
///
/// Two of these is a faithful stand-in for two computers, because the clients
/// only ever communicate through the server.
///
/// The tests share one long-lived server, so each works inside its own
/// directory on it — a *scope*. Files from other tests still sync down, since
/// a real client syncs the whole tree, so every assertion about local contents
/// is filtered to the scope rather than taken over the whole folder.
struct TestMachine {
    let scope: String
    let root: URL
    let stateURL: URL
    let store: FileStore
    let engine: SyncEngine
    let server: ServerProbe

    static func newScope() -> String {
        "scope-\(UUID().uuidString.prefix(8))"
    }

    init(
        scope: String = TestMachine.newScope(),
        device: String = "machine",
        maxDeletes: Int = 100
    ) throws {
        guard let serverURL = TestServer.url, let token = TestServer.token else {
            throw TestServerUnavailable()
        }

        self.scope = scope

        let unique = UUID().uuidString.prefix(8)
        self.root = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "homesync-\(scope)-\(device)-\(unique)")
        self.stateURL = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "homesync-state-\(scope)-\(device)-\(unique)")
            .appending(path: "state.sqlite")

        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)

        let configuration = Configuration(
            serverURL: serverURL,
            token: token,
            root: root,
            stateURL: stateURL,
            deviceName: device,
            maxDeletesPerPull: maxDeletes,
            // Absolute count only. The fraction is relative to everything the
            // machine has seen, including other tests' files, which would make
            // the threshold depend on execution order.
            maxDeleteFraction: 1.0
        )

        self.store = try FileStore(root: root)
        self.engine = try SyncEngine(configuration: configuration)
        self.server = ServerProbe(api: APIClient(baseURL: serverURL, token: token))
    }

    private init(
        scope: String, root: URL, stateURL: URL, store: FileStore,
        engine: SyncEngine, server: ServerProbe
    ) {
        self.scope = scope
        self.root = root
        self.stateURL = stateURL
        self.store = store
        self.engine = engine
        self.server = server
    }

    /// A fresh engine over the same folder and the same state database, which
    /// is what a restart looks like.
    func restarted() throws -> TestMachine {
        guard let serverURL = TestServer.url, let token = TestServer.token else {
            throw TestServerUnavailable()
        }

        let configuration = Configuration(
            serverURL: serverURL, token: token, root: root,
            stateURL: stateURL, deviceName: "restarted")

        return TestMachine(
            scope: scope, root: root, stateURL: stateURL, store: store,
            engine: try SyncEngine(configuration: configuration), server: server)
    }

    /// A fresh engine over the same folder but an *empty* state database.
    ///
    /// What the app looked like when the state file was keyed by `hashValue`:
    /// the folder is untouched and full, and the engine has no memory of ever
    /// having synced any of it.
    func withForgottenState() throws -> TestMachine {
        guard let serverURL = TestServer.url, let token = TestServer.token else {
            throw TestServerUnavailable()
        }

        let fresh = stateURL
            .deletingLastPathComponent()
            .appending(path: "forgotten-\(UUID().uuidString.prefix(8)).sqlite")

        let configuration = Configuration(
            serverURL: serverURL, token: token, root: root,
            stateURL: fresh, deviceName: "forgetful", maxDeleteFraction: 1.0)

        return TestMachine(
            scope: scope, root: root, stateURL: fresh, store: store,
            engine: try SyncEngine(configuration: configuration), server: server)
    }

    /// The full sync path for a name inside this machine's scope.
    func scoped(_ path: String) -> String { "\(scope)/\(path)" }

    // MARK: - Local side

    func write(_ path: String, _ content: String) throws {
        let url = store.url(for: scoped(path))
        try FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        try content.write(to: url, atomically: true, encoding: .utf8)
    }

    func read(_ path: String) -> String? {
        try? String(contentsOf: store.url(for: scoped(path)), encoding: .utf8)
    }

    func exists(_ path: String) -> Bool {
        store.exists(scoped(path))
    }

    func remove(_ path: String) throws {
        try FileManager.default.removeItem(at: store.url(for: scoped(path)))
    }

    func touch(_ path: String, at date: Date) throws {
        try FileManager.default.setAttributes(
            [.modificationDate: date], ofItemAtPath: store.url(for: scoped(path)).path)
    }

    /// Regular files inside this machine's scope, as names relative to it.
    func files() -> [String] {
        guard let found = try? store.scan(ignoring: IgnoreRules(rules: "")) else { return [] }
        let prefix = "\(scope)/"
        return found.values
            .filter { $0.type == .file && $0.path.hasPrefix(prefix) }
            .map { String($0.path.dropFirst(prefix.count)) }
            .sorted()
    }

    /// The contents of every file in this scope, for asserting that nothing was
    /// lost without caring which name it ended up under.
    func allContents() -> Set<String> {
        Set(files().compactMap { read($0) })
    }
}
