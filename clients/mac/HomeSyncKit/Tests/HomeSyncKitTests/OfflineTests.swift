import Foundation
import Testing

@testable import HomeSyncKit

/// Stands in for a network that is not there.
///
/// Every request fails with `networkConnectionLost` — code -1005, the one a
/// laptop reports after leaving the house or dropping the VPN, and the one
/// that filled the menu bar with `_kCFStreamErrorCodeKey`.
///
/// It fails slowly on purpose. A doomed request in the real world takes as
/// long as a connection attempt, and the bug being tested here is about what
/// the engine says *while* it is trying, so the attempt has to last long
/// enough to look at. Sleeping the loading thread rather than scheduling the
/// failure keeps the whole thing free of shared state.
private final class UnreachableNetwork: URLProtocol {
    static let attemptTakes: TimeInterval = 0.2

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Thread.sleep(forTimeInterval: Self.attemptTakes)
        client?.urlProtocol(self, didFailWithError: URLError(.networkConnectionLost))
    }

    override func stopLoading() {}
}

/// What the engine says when it cannot reach the server at all.
///
/// These need no server: the point is the absence of one. They are the only
/// tests here that run on a machine with nothing to sync against.
@Suite("Losing the network")
struct OfflineTests {
    private static func engine() throws -> SyncEngine {
        let unique = UUID().uuidString.prefix(8)
        let root = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "homesync-offline-\(unique)")
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)

        let configuration = Configuration(
            serverURL: URL(string: "http://10.0.20.10:8420")!,
            token: "unusable",
            root: root,
            // Outside the synced folder, exactly as in production: a state
            // database inside the root would sync itself.
            stateURL: URL(fileURLWithPath: NSTemporaryDirectory())
                .appending(path: "homesync-offline-state-\(unique)")
                .appending(path: "state.sqlite"),
            deviceName: "offline")

        let session = URLSessionConfiguration.ephemeral
        session.protocolClasses = [UnreachableNetwork.self]

        return try SyncEngine(
            configuration: configuration, session: URLSession(configuration: session))
    }

    private static func isSyncing(_ state: SyncState) -> Bool {
        if case .syncing = state { return true }
        return false
    }

    @Test("a server it cannot reach is reported as being out of reach")
    func reportsOffline() async throws {
        let engine = try Self.engine()

        _ = try? await engine.syncOnce()

        guard case .offline(let reason) = await engine.currentState else {
            Issue.record("expected offline, got \(await engine.currentState)")
            return
        }

        // The menu bar has room for a sentence, and it showed a page: the
        // whole of NSError's description, down to the peer address as hex.
        #expect(!reason.contains("NSURLErrorDomain"))
        #expect(!reason.contains("Error Domain="))
        #expect(reason.count < 120, "too long for the menu to show: \(reason)")
    }

    @Test("retrying against an unreachable server does not claim to be syncing")
    func staysOfflineWhileRetrying() async throws {
        let engine = try Self.engine()
        _ = try? await engine.syncOnce()

        // The retry is the whole bug. A failed cycle is repeated at once, and
        // it used to announce itself as syncing before it could fail — so with
        // the network down the menu bar said "Syncing…", forever, and the
        // reason was published for microseconds at a time in between.
        let retry = Task { try? await engine.syncOnce() }

        var seen: [SyncState] = []
        for _ in 0..<10 {
            try await Task.sleep(for: .milliseconds(25))
            seen.append(await engine.currentState)
        }
        _ = await retry.value

        #expect(!seen.contains(where: Self.isSyncing), "said it was syncing: \(seen)")
        #expect(seen.allSatisfy { if case .offline = $0 { return true } else { return false } },
                "should have stayed offline throughout: \(seen)")
    }

    @Test("a dropped event stream is reported in words, not as a URLError")
    func reportsEventStreamFailureInWords() async throws {
        let engine = try Self.engine()

        // The reported screenshot: nothing had been touched locally, so the
        // event stream reconnecting was the only thing running, and it printed
        // its raw NSError into the menu.
        let running = Task { await engine.run() }
        defer { running.cancel() }
        try await Task.sleep(for: .seconds(2))

        guard case .offline(let reason) = await engine.currentState else {
            Issue.record("expected offline, got \(await engine.currentState)")
            return
        }
        #expect(!reason.contains("Error Domain="))
    }
}
