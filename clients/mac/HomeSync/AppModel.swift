import Foundation
import HomeSyncKit
import Observation
import ServiceManagement
import SwiftUI

/// Bridges the engine to the interface.
///
/// The engine is an actor, so its state cannot be read directly from a SwiftUI
/// view. This polls it on the main actor and republishes what the menu needs.
/// Polling rather than pushing keeps the engine free of any knowledge that a
/// user interface exists.
@MainActor
@Observable
final class AppModel {
    // MARK: - Settings

    var serverURL: String {
        didSet { UserDefaults.standard.set(serverURL, forKey: "serverURL") }
    }

    var syncFolder: String {
        didSet { UserDefaults.standard.set(syncFolder, forKey: "syncFolder") }
    }

    /// Held in the Keychain, never in defaults.
    var token: String {
        didSet { Keychain.writeToken(token) }
    }

    // MARK: - Observed state

    private(set) var state: SyncState = .idle(lastSync: nil)
    private(set) var fileCount = 0
    private(set) var conflicts: [String] = []
    private(set) var isConfigured = false
    private(set) var isInsecure = false

    var launchesAtLogin: Bool {
        get { SMAppService.mainApp.status == .enabled }
        set { setLaunchAtLogin(newValue) }
    }

    private var engine: SyncEngine?
    private var engineTask: Task<Void, Never>?
    private var pollTask: Task<Void, Never>?

    static let defaultFolder = FileManager.default
        .homeDirectoryForCurrentUser
        .appending(path: "Library/CloudStorage/HomeSync", directoryHint: .isDirectory)
        .path

    init() {
        let defaults = UserDefaults.standard
        serverURL = defaults.string(forKey: "serverURL") ?? ""
        syncFolder = defaults.string(forKey: "syncFolder") ?? Self.defaultFolder
        token = Keychain.readToken() ?? ""
    }

    // MARK: - Lifecycle

    /// Starts syncing, or stops and reports why it cannot.
    func start() {
        stop()

        guard !serverURL.isEmpty, !token.isEmpty, let url = URL(string: serverURL) else {
            isConfigured = false
            state = .paused(reason: "Not configured yet. Open Settings to add the server and token.")
            return
        }

        let root = URL(fileURLWithPath: syncFolder, isDirectory: true)
        let configuration = Configuration(serverURL: url, token: token, root: root)

        do {
            let engine = try SyncEngine(configuration: configuration)
            self.engine = engine
            isConfigured = true

            createConvenienceSymlink(to: root)

            engineTask = Task { await engine.run() }
            pollTask = Task { [weak self] in await self?.pollStatus(of: engine) }
        } catch {
            isConfigured = false
            state = .failed("Cannot start: \(error)")
        }
    }

    func stop() {
        engineTask?.cancel()
        pollTask?.cancel()
        engineTask = nil
        pollTask = nil
        engine = nil
    }

    /// Applies changed settings by restarting cleanly, which is simpler to
    /// reason about than mutating a running engine's configuration.
    func restart() {
        start()
    }

    func syncNow() {
        guard let engine else { return }
        Task { try? await engine.syncOnce() }
    }

    func clearConflicts() {
        conflicts = []
        guard let engine else { return }
        Task { await engine.clearConflicts() }
    }

    func revealInFinder(_ path: String) {
        let url = URL(fileURLWithPath: syncFolder).appending(path: path)
        NSWorkspace.shared.activateFileViewerSelecting([url])
    }

    func openSyncFolder() {
        NSWorkspace.shared.open(URL(fileURLWithPath: syncFolder))
    }

    private func pollStatus(of engine: SyncEngine) async {
        while !Task.isCancelled {
            state = await engine.currentState
            conflicts = await engine.conflicts
            fileCount = await engine.syncedFileCount
            isInsecure = await engine.isTransportInsecure

            try? await Task.sleep(for: .seconds(1))
        }
    }

    // MARK: - Login item

    private func setLaunchAtLogin(_ enabled: Bool) {
        do {
            if enabled {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
        } catch {
            state = .failed("Cannot change the login item: \(error.localizedDescription)")
        }
    }

    /// `~/Library/CloudStorage` is buried and awkward to reach. A symlink in the
    /// home folder makes the synced files easy to get at from a terminal or the
    /// Finder's Go menu.
    private func createConvenienceSymlink(to root: URL) {
        let link = FileManager.default.homeDirectoryForCurrentUser.appending(path: "HomeSync")

        guard !FileManager.default.fileExists(atPath: link.path) else { return }
        try? FileManager.default.createSymbolicLink(at: link, withDestinationURL: root)
    }
}

// MARK: - Presentation

extension SyncState {
    /// SF Symbol for the menu bar. The icon is the whole interface most of the
    /// time, so it has to carry the state on its own.
    var symbolName: String {
        switch self {
        case .idle: return "checkmark.circle"
        case .syncing: return "arrow.triangle.2.circlepath"
        case .paused: return "pause.circle"
        case .failed: return "exclamationmark.triangle"
        }
    }

    var summary: String {
        switch self {
        case .idle(let lastSync):
            guard let lastSync else { return "Ready" }
            return "Up to date · \(Self.relative(lastSync))"
        case .syncing:
            return "Syncing…"
        case .paused(let reason):
            return reason
        case .failed(let reason):
            return reason
        }
    }

    var needsAttention: Bool {
        switch self {
        case .paused, .failed: return true
        case .idle, .syncing: return false
        }
    }

    private static func relative(_ date: Date) -> String {
        let formatter = RelativeDateTimeFormatter()
        formatter.unitsStyle = .abbreviated
        return formatter.localizedString(for: date, relativeTo: Date())
    }
}
