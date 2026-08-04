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
    log("connection is unencrypted, which is expected on a home network; "
        + "use https:// if this server is reachable from outside it")
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
    // What is wrong is printed when it changes, not every two seconds. Being
    // off the network lasts as long as the walk to the car, and a line per
    // tick would bury everything else in the log.
    var lastProblem: String?

    func report(problem: String) {
        guard problem != lastProblem else { return }
        lastProblem = problem
        log(problem)
    }

    while !Task.isCancelled {
        try? await Task.sleep(for: .seconds(2))

        let state = await engine.currentState
        if case .idle(let at) = state, let at, at != lastReported {
            lastReported = at
            lastProblem = nil
            let conflicts = await engine.conflicts
            let count = await engine.syncedFileCount
            if conflicts.isEmpty {
                log("up to date · \(count) files")
            } else {
                log("up to date · \(count) files · \(conflicts.count) conflicts: "
                    + conflicts.suffix(3).joined(separator: ", "))
            }
        } else if case .syncing(let progress) = state, let progress,
                  let pct = progress.percentage {
            log("\(progress.phase.verb) \(progress.completed)/\(progress.total) · \(pct)%")
        } else if case .offline(let reason) = state {
            report(problem: "offline: \(reason)")
        } else if case .paused(let reason) = state {
            report(problem: "PAUSED: \(reason)")
        } else if case .failed(let reason) = state {
            report(problem: "error: \(reason)")
        }
    }
}

await engine.run()
reporter.cancel()
