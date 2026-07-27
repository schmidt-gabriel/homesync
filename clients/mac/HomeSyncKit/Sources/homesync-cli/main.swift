import Foundation
import HomeSyncKit

// A headless client. It exists so the engine can be driven before any interface
// does, which is what lets the conformance suite validate it, and so a sync
// problem can be reproduced without launching the app.

let environment = ProcessInfo.processInfo.environment

func requireEnvironment(_ key: String) -> String {
    guard let value = environment[key], !value.isEmpty else {
        FileHandle.standardError.write(Data("""
            homesync-cli — headless HomeSync client

            Required environment:
              HOMESYNC_URL     server to sync with, e.g. http://localhost:8420
              HOMESYNC_TOKEN   device token (homesync device add <name>)
              HOMESYNC_ROOT    folder to keep in sync

            Optional:
              HOMESYNC_STATE   where to keep the sync state database
              HOMESYNC_DEVICE  name used in conflict copies (default: hostname)
              HOMESYNC_ONCE    set to run a single cycle and exit

            Missing: \(key)

            """.utf8))
        exit(2)
    }
    return value
}

let serverText = requireEnvironment("HOMESYNC_URL")
let token = requireEnvironment("HOMESYNC_TOKEN")
let rootPath = requireEnvironment("HOMESYNC_ROOT")

guard let serverURL = URL(string: serverText) else {
    FileHandle.standardError.write(Data("HOMESYNC_URL is not a valid URL: \(serverText)\n".utf8))
    exit(2)
}

let configuration = Configuration(
    serverURL: serverURL,
    token: token,
    root: URL(fileURLWithPath: rootPath, isDirectory: true),
    stateURL: environment["HOMESYNC_STATE"].map { URL(fileURLWithPath: $0) },
    deviceName: environment["HOMESYNC_DEVICE"] ?? Configuration.defaultDeviceName
)

func log(_ message: String) {
    let stamp = ISO8601DateFormatter().string(from: Date())
    print("[\(stamp)] \(message)")
    fflush(stdout)
}

let engine: SyncEngine
do {
    engine = try SyncEngine(configuration: configuration)
} catch {
    FileHandle.standardError.write(Data("cannot start: \(error)\n".utf8))
    exit(1)
}

if await engine.isTransportInsecure {
    log("warning: \(serverURL.scheme ?? "http") is not encrypted, so the device token "
        + "travels in the clear. Fine on a trusted network, not beyond it.")
}

if environment["HOMESYNC_ONCE"] != nil {
    do {
        await engine.refreshIgnoreRules()
        let summary = try await engine.syncOnce()
        log("done: \(summary)")
        exit(0)
    } catch {
        FileHandle.standardError.write(Data("sync failed: \(error)\n".utf8))
        exit(1)
    }
}

log("syncing \(rootPath) with \(serverURL)")

// Report each cycle's outcome without polling the engine into the ground: a
// summary line only when something actually happened.
let reporter = Task {
    var lastReported: Date?
    while !Task.isCancelled {
        try? await Task.sleep(for: .seconds(2))

        let state = await engine.currentState
        if case .idle(let at) = state, let at, at != lastReported {
            lastReported = at
            let conflicts = await engine.conflicts
            let count = await engine.syncedFileCount
            if conflicts.isEmpty {
                log("up to date · \(count) files")
            } else {
                log("up to date · \(count) files · \(conflicts.count) conflicts: "
                    + conflicts.suffix(3).joined(separator: ", "))
            }
        } else if case .paused(let reason) = state {
            log("PAUSED: \(reason)")
        } else if case .failed(let reason) = state {
            log("error: \(reason)")
        }
    }
}

await engine.run()
reporter.cancel()
