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

    /// The outcome of the last attempt to reach the server from Settings.
    ///
    /// Saving a URL and a token is not the same as them working, and the
    /// difference is exactly what someone wants to know at the moment they
    /// press Apply. The engine's own status eventually says the same thing,
    /// but only after a sync cycle and only in the menu.
    enum ConnectionCheck: Equatable {
        case none
        case checking
        case connected
        case failed(String)
    }

    private(set) var connectionCheck: ConnectionCheck = .none

    /// Whether the app is registered to open at login.
    ///
    /// Stored, rather than read straight off `SMAppService.mainApp.status` in a
    /// computed property. `@Observable` can only track its own storage: reading
    /// a service outside the object changes nothing the view is observing, so
    /// the toggle registered the login item and then went on drawing the value
    /// from its last render — off. Only `refreshLaunchesAtLogin()` writes this,
    /// and it writes what the service reports rather than what was asked of it.
    private(set) var launchesAtLogin = false

    /// Why the toggle did not do what it appears to have done, when that
    /// happens. Registering can fail, and it can also succeed while leaving the
    /// item off, which is indistinguishable from nothing happening at all.
    private(set) var loginItemProblem: String?

    private var engine: SyncEngine?
    private var engineTask: Task<Void, Never>?
    private var pollTask: Task<Void, Never>?

    /// What the bundle says it is, such as "1.0.3".
    ///
    /// Read from the bundle rather than written in code: the two would drift,
    /// and the version is the first thing worth knowing when someone reports a
    /// problem. CI stamps it from the release tag.
    ///
    /// The build number is deliberately left out. It is a CI run counter and
    /// means nothing to anyone reading it; the release version is what
    /// identifies a build.
    static let version: String =
        Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String
        ?? "unknown"

    static let defaultFolder = FileManager.default
        .homeDirectoryForCurrentUser
        .appending(path: "Library/CloudStorage/HomeSync", directoryHint: .isDirectory)
        .path

    init() {
        let defaults = UserDefaults.standard
        serverURL = defaults.string(forKey: "serverURL") ?? ""
        syncFolder = defaults.string(forKey: "syncFolder") ?? Self.defaultFolder
        token = Keychain.readToken() ?? ""

        refreshLaunchesAtLogin()
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

    /// Saves new server settings and reports whether they actually work.
    ///
    /// The settings are stored either way: they are the user's input, and
    /// refusing to keep them because the server happens to be down right now
    /// would be worse than telling them what went wrong.
    func applyServerSettings(url rawURL: String, token newToken: String) async {
        let trimmedURL = rawURL.trimmingCharacters(in: .whitespaces)
        let trimmedToken = newToken.trimmingCharacters(in: .whitespaces)

        serverURL = trimmedURL
        token = trimmedToken
        connectionCheck = .checking

        guard let url = URL(string: trimmedURL), url.host != nil else {
            connectionCheck = .failed("That does not look like a URL.")
            return
        }

        // One cheap authenticated request answers everything that can be wrong
        // at this point: unreachable host, wrong port, bad token.
        let client = APIClient(baseURL: url, token: trimmedToken)
        do {
            _ = try await client.changes(since: 0, limit: 1)
            connectionCheck = .connected
            restart()
        } catch {
            connectionCheck = .failed(describe(error))
            restart()
        }
    }

    /// Turns a failure into something worth reading. `SyncError` already
    /// describes itself well; the two cases worth rewording are the ones a
    /// person can actually act on.
    private func describe(_ error: any Error) -> String {
        guard let syncError = error as? SyncError else {
            return error.localizedDescription
        }

        switch syncError {
        case .unauthorized:
            return "The server rejected this token. Create a new one and paste it here."
        case .transport:
            return "Could not reach the server. Check the address, the port, and that it is running."
        default:
            return String(describing: syncError)
        }
    }

    func clearConnectionCheck() {
        connectionCheck = .none
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

    /// How long the syncing state stays on screen once it has been seen.
    ///
    /// A cycle against a server on the same network can finish in tens of
    /// milliseconds. Reported honestly, the icon would flick between states too
    /// fast to read, or be missed between polls entirely — so the indicator
    /// would be permanently still while the thing it indicates is working. It
    /// is held briefly so that "something is happening" is legible.
    private static let syncingVisibleFor: Duration = .milliseconds(700)

    private var syncingUntil: ContinuousClock.Instant?

    private func pollStatus(of engine: SyncEngine) async {
        let clock = ContinuousClock()

        while !Task.isCancelled {
            let actual = await engine.currentState

            if case .syncing = actual {
                syncingUntil = clock.now.advanced(by: Self.syncingVisibleFor)
            }

            // Anything other than idle wins over the latch: an error or a pause
            // is more important than finishing the animation.
            if case .idle = actual, let until = syncingUntil, clock.now < until {
                state = .syncing(progress: nil)
            } else {
                syncingUntil = nil
                state = actual
            }

            conflicts = await engine.conflicts
            fileCount = await engine.syncedFileCount
            isInsecure = await engine.isTransportInsecure

            // Fast enough to catch a short cycle at all. It only reads a few
            // properties off an actor, so the cost is negligible.
            try? await Task.sleep(for: .milliseconds(150))
        }
    }

    // MARK: - Login item

    func setLaunchesAtLogin(_ enabled: Bool) {
        do {
            if enabled {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
            refreshLaunchesAtLogin()
        } catch {
            refreshLaunchesAtLogin()
            loginItemProblem = "Cannot change the login item: \(error.localizedDescription)"
        }
    }

    /// Asks the service what the registration actually is, and republishes it.
    ///
    /// Worth calling whenever Settings appears, not only after the toggle:
    /// System Settings › General › Login Items is the other place this changes,
    /// and it can change there while the app is running.
    func refreshLaunchesAtLogin() {
        let status = SMAppService.mainApp.status
        launchesAtLogin = status == .enabled

        // `register()` returns without complaint when the user has switched the
        // item off in System Settings, and the item stays off: the decision is
        // remembered, and only they can reverse it, in that window.
        loginItemProblem = status == .requiresApproval
            ? "HomeSync was switched off in System Settings, so it has to be turned back on there."
            : nil
    }

    func openLoginItemsSettings() {
        SMAppService.openSystemSettingsLoginItems()
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
        case .offline: return "wifi.slash"
        case .paused: return "pause.circle"
        case .failed: return "exclamationmark.triangle"
        }
    }

    var summary: String {
        switch self {
        case .idle(let lastSync):
            guard let lastSync else { return "Ready" }
            return "Up to date · \(Self.relative(lastSync))"
        case .syncing(let progress):
            guard let progress, let percentage = progress.percentage else {
                return "Syncing…"
            }
            return "\(progress.phase.verb) \(progress.completed) of \(progress.total) · \(percentage)%"
        case .offline(let reason):
            return reason
        case .paused(let reason):
            return reason
        case .failed(let reason):
            return reason
        }
    }

    var needsAttention: Bool {
        switch self {
        case .offline, .paused, .failed: return true
        case .idle, .syncing: return false
        }
    }

    private static func relative(_ date: Date) -> String {
        let formatter = RelativeDateTimeFormatter()
        formatter.unitsStyle = .abbreviated
        return formatter.localizedString(for: date, relativeTo: Date())
    }
}
