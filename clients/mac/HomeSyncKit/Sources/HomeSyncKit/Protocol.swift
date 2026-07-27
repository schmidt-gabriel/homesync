import Foundation

/// The wire types from `docs/PROTOCOL.md`. Names and semantics come from that
/// document; if the two ever disagree, the document is right.
public enum EntryType: String, Codable, Sendable {
    case file
    case dir
}

/// One path at one revision, as the server describes it.
public struct RemoteEntry: Codable, Sendable, Equatable {
    public let path: String
    public let type: EntryType
    public let size: Int64
    /// Unix milliseconds, not seconds.
    public let mtime: Int64
    public let sha256: String?
    public let rev: Int64
    public let deleted: Bool
    /// Present when the name cannot be represented on Windows.
    public let unsafe: Bool

    public init(
        path: String, type: EntryType, size: Int64, mtime: Int64,
        sha256: String?, rev: Int64, deleted: Bool, unsafe: Bool = false
    ) {
        self.path = path
        self.type = type
        self.size = size
        self.mtime = mtime
        self.sha256 = sha256
        self.rev = rev
        self.deleted = deleted
        self.unsafe = unsafe
    }

    private enum CodingKeys: String, CodingKey {
        case path, type, size, mtime, sha256, rev, deleted, unsafe
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        path = try container.decode(String.self, forKey: .path)
        type = try container.decode(EntryType.self, forKey: .type)
        size = try container.decodeIfPresent(Int64.self, forKey: .size) ?? 0
        mtime = try container.decodeIfPresent(Int64.self, forKey: .mtime) ?? 0
        sha256 = try container.decodeIfPresent(String.self, forKey: .sha256)
        rev = try container.decode(Int64.self, forKey: .rev)
        deleted = try container.decodeIfPresent(Bool.self, forKey: .deleted) ?? false
        unsafe = try container.decodeIfPresent(Bool.self, forKey: .unsafe) ?? false
    }
}

/// One page of `GET /v1/changes`.
public struct ChangesPage: Codable, Sendable {
    public let changes: [RemoteEntry]
    public let currentRev: Int64
    /// True when the page was truncated: ask again from the last entry's rev.
    public let more: Bool

    private enum CodingKeys: String, CodingKey {
        case changes
        case currentRev = "current_rev"
        case more
    }
}

/// What the server returns after a successful PUT or DELETE.
public struct FileResponse: Codable, Sendable {
    public let path: String
    public let rev: Int64
    public let size: Int64
    public let sha256: String?
    public let mtime: Int64
    public let type: EntryType

    private enum CodingKeys: String, CodingKey {
        case path, rev, size, sha256, mtime, type
    }

    public init(from decoder: any Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        path = try container.decode(String.self, forKey: .path)
        rev = try container.decode(Int64.self, forKey: .rev)
        size = try container.decodeIfPresent(Int64.self, forKey: .size) ?? 0
        sha256 = try container.decodeIfPresent(String.self, forKey: .sha256)
        mtime = try container.decodeIfPresent(Int64.self, forKey: .mtime) ?? 0
        type = try container.decodeIfPresent(EntryType.self, forKey: .type) ?? .file
    }
}

/// The single error shape every endpoint uses.
public struct ServerError: Codable, Sendable {
    public let error: String
    public let message: String
    /// Set when `error` is `conflict`: the name the losing content was
    /// stored under.
    public let conflict: String?
}

/// The shared ignore rules.
public struct IgnoreDocument: Codable, Sendable {
    public let rules: String
    public let version: Int64
}

/// Everything that can go wrong talking to a server.
public enum SyncError: Error, Sendable {
    /// The path changed underneath us. `conflict` carries the name the server
    /// parked our content under, when it had one.
    case conflict(path: String, conflict: String?, message: String)
    /// We tried to modify a revision that is no longer current.
    case stale(path: String, message: String)
    case notFound(path: String)
    case unauthorized
    case server(status: Int, code: String, message: String)
    case transport(any Error)
    case invalidResponse(String)

    /// Whether retrying the identical request could plausibly work. A bad
    /// token or a rejected path will fail again just as fast.
    public var isRetryable: Bool {
        switch self {
        case .transport:
            return true
        case .server(let status, _, _):
            return status >= 500 || status == 429
        case .conflict, .stale, .notFound, .unauthorized, .invalidResponse:
            return false
        }
    }
}

extension SyncError: CustomStringConvertible {
    public var description: String {
        switch self {
        case .conflict(let path, let conflict, _):
            let stored = conflict.map { " (stored as \($0))" } ?? ""
            return "conflict on \(path)\(stored)"
        case .stale(let path, _):
            return "\(path) changed on the server; fetch it first"
        case .notFound(let path):
            return "\(path) not found"
        case .unauthorized:
            return "the server rejected this device's token"
        case .server(let status, let code, let message):
            return "server error \(status) \(code): \(message)"
        case .transport(let underlying):
            return "network: \(underlying.localizedDescription)"
        case .invalidResponse(let detail):
            return "unexpected response: \(detail)"
        }
    }
}
