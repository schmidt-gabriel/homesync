import Foundation

/// Everything the engine needs to run.
public struct Configuration: Sendable {
    public let serverURL: URL
    public let token: String
    /// The folder kept in sync.
    public let root: URL
    /// Where the engine's record of the last confirmed sync lives. Kept out of
    /// the synced folder, or it would sync itself.
    public let stateURL: URL
    /// Names the conflict copies this machine creates, so it is obvious later
    /// which machine produced one.
    public let deviceName: String

    /// A safety net, not a tuning knob.
    ///
    /// A corrupted state database, or a server whose volume failed to mount,
    /// can produce a change set that deletes everything. The engine refuses to
    /// apply a pull that would remove more than this and reports it instead.
    /// It costs nothing until the day it saves everything.
    public let maxDeletesPerPull: Int
    public let maxDeleteFraction: Double

    /// Backstop for the event stream. Real-time updates arrive over SSE; this
    /// only matters when that connection is down.
    public let pollInterval: Duration

    public init(
        serverURL: URL,
        token: String,
        root: URL,
        stateURL: URL? = nil,
        deviceName: String = Configuration.defaultDeviceName,
        maxDeletesPerPull: Int = 100,
        maxDeleteFraction: Double = 0.25,
        pollInterval: Duration = .seconds(300)
    ) {
        self.serverURL = serverURL
        self.token = token
        self.root = root
        self.stateURL = stateURL ?? Configuration.defaultStateURL(for: root)
        self.deviceName = deviceName
        self.maxDeletesPerPull = maxDeletesPerPull
        self.maxDeleteFraction = maxDeleteFraction
        self.pollInterval = pollInterval
    }

    public static var defaultDeviceName: String {
        ProcessInfo.processInfo.hostName
            .replacingOccurrences(of: ".local", with: "")
    }

    /// One state database per sync root, keyed by a hash of its path so two
    /// roots on one machine cannot share, or corrupt, each other's record.
    public static func defaultStateURL(for root: URL) -> URL {
        let support = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)
            .first ?? URL(fileURLWithPath: NSTemporaryDirectory())

        let key = String(UInt64(bitPattern: Int64(root.standardizedFileURL.path.hashValue)), radix: 36)

        return support
            .appending(path: "HomeSync", directoryHint: .isDirectory)
            .appending(path: "state-\(key).sqlite", directoryHint: .notDirectory)
    }
}

/// What one sync cycle did.
public struct SyncSummary: Sendable, Equatable {
    public var downloaded = 0
    public var uploaded = 0
    public var deletedLocally = 0
    public var deletedRemotely = 0
    public var directoriesCreated = 0
    /// Paths where both sides had changed. Worth surfacing to the user rather
    /// than logging: a conflict needs a human to resolve it.
    public var conflicts: [String] = []

    public var isEmpty: Bool {
        downloaded == 0 && uploaded == 0 && deletedLocally == 0
            && deletedRemotely == 0 && directoriesCreated == 0 && conflicts.isEmpty
    }

    static func + (lhs: SyncSummary, rhs: SyncSummary) -> SyncSummary {
        SyncSummary(
            downloaded: lhs.downloaded + rhs.downloaded,
            uploaded: lhs.uploaded + rhs.uploaded,
            deletedLocally: lhs.deletedLocally + rhs.deletedLocally,
            deletedRemotely: lhs.deletedRemotely + rhs.deletedRemotely,
            directoriesCreated: lhs.directoriesCreated + rhs.directoriesCreated,
            conflicts: lhs.conflicts + rhs.conflicts
        )
    }
}

extension SyncSummary: CustomStringConvertible {
    public var description: String {
        if isEmpty { return "nothing to do" }

        var parts: [String] = []
        if downloaded > 0 { parts.append("↓\(downloaded)") }
        if uploaded > 0 { parts.append("↑\(uploaded)") }
        if deletedLocally > 0 { parts.append("-\(deletedLocally) local") }
        if deletedRemotely > 0 { parts.append("-\(deletedRemotely) remote") }
        if directoriesCreated > 0 { parts.append("+\(directoriesCreated) dirs") }
        if !conflicts.isEmpty { parts.append("\(conflicts.count) conflicts") }
        return parts.joined(separator: " ")
    }
}

/// How far through the current cycle the engine is.
///
/// Reported per phase rather than as one number for the whole cycle: how much
/// there is to upload is not known until the download phase has finished, so a
/// single combined total would have to be guessed and then revised downwards,
/// which is worse than two honest ones.
public struct SyncProgress: Sendable, Equatable {
    public enum Phase: Sendable, Equatable {
        case downloading
        case uploading

        public var verb: String {
            switch self {
            case .downloading: return "Downloading"
            case .uploading: return "Uploading"
            }
        }
    }

    public let phase: Phase
    public let completed: Int
    public let total: Int

    public init(phase: Phase, completed: Int, total: Int) {
        self.phase = phase
        self.completed = completed
        self.total = total
    }

    /// 0 to 1, or nil when there is nothing to be a fraction of.
    public var fraction: Double? {
        guard total > 0 else { return nil }
        return min(Double(completed) / Double(total), 1)
    }

    public var percentage: Int? {
        guard let fraction else { return nil }
        return Int(fraction * 100)
    }
}

/// What the engine is doing, for a user interface to show.
public enum SyncState: Sendable, Equatable {
    case idle(lastSync: Date?)
    /// `progress` is nil for a cycle small enough that counting it would be
    /// noise, and for the moment before the work is known.
    case syncing(progress: SyncProgress?)
    case paused(reason: String)
    case failed(String)
}
