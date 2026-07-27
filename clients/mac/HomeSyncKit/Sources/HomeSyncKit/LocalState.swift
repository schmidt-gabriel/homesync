import Foundation
import SQLite3

/// SQLite wants to know whether it may keep a borrowed pointer. Our strings are
/// Swift-owned and can move, so it must copy.
private let SQLITE_TRANSIENT = unsafeBitCast(-1, to: sqlite3_destructor_type.self)

/// What we knew about a path the last time it synced cleanly.
///
/// This is the third reference point that makes bidirectional sync possible.
/// Comparing the disk against the server tells you they differ; comparing both
/// against this tells you *which one* changed, and therefore whether to push,
/// pull, or declare a conflict.
public struct SyncedState: Sendable, Equatable {
    public let path: String
    public let type: EntryType
    public let size: Int64
    public let mtime: Int64
    public let sha256: String
    public let rev: Int64

    public init(path: String, type: EntryType, size: Int64, mtime: Int64, sha256: String, rev: Int64) {
        self.path = path
        self.type = type
        self.size = size
        self.mtime = mtime
        self.sha256 = sha256
        self.rev = rev
    }
}

public enum LocalStateError: Error, CustomStringConvertible {
    case open(String)
    case query(String)

    public var description: String {
        switch self {
        case .open(let detail): return "cannot open the local state database: \(detail)"
        case .query(let detail): return "local state query failed: \(detail)"
        }
    }
}

/// The client's record of the last confirmed sync.
///
/// Not `Sendable` on purpose: the SQLite handle has a single owner, the
/// ``SyncEngine`` actor, and the compiler enforces that rather than trusting a
/// convention.
public final class LocalState {
    private var db: OpaquePointer?

    /// Opens, or creates, the database at `url`.
    public init(at url: URL) throws {
        try FileManager.default.createDirectory(
            at: url.deletingLastPathComponent(), withIntermediateDirectories: true)

        guard sqlite3_open(url.path, &db) == SQLITE_OK, db != nil else {
            // sqlite3_open hands back a handle even on failure, purely so the
            // error can be read off it before closing.
            let message = db.map { String(cString: sqlite3_errmsg($0)) } ?? "unknown"
            sqlite3_close(db)
            self.db = nil
            throw LocalStateError.open(message)
        }

        // WAL survives an unclean shutdown without losing committed work, which
        // matters for a process the user may quit at any moment.
        try execute("PRAGMA journal_mode = WAL")
        try execute("PRAGMA synchronous = NORMAL")
        try execute("PRAGMA busy_timeout = 5000")

        try execute("""
            CREATE TABLE IF NOT EXISTS state (
                path   TEXT PRIMARY KEY,
                type   TEXT NOT NULL,
                size   INTEGER NOT NULL,
                mtime  INTEGER NOT NULL,
                sha256 TEXT NOT NULL DEFAULT '',
                rev    INTEGER NOT NULL
            )
            """)
        try execute("""
            CREATE TABLE IF NOT EXISTS meta (
                key   TEXT PRIMARY KEY,
                value TEXT NOT NULL
            )
            """)
    }

    deinit {
        sqlite3_close(db)
    }

    // MARK: - Revision cursor

    /// The last revision fully applied. One number is the client's entire
    /// notion of "where I am".
    public var lastRev: Int64 {
        get { Int64(meta("last_rev") ?? "") ?? 0 }
        set { try? setMeta("last_rev", String(newValue)) }
    }

    /// Version of the shared ignore rules we last fetched.
    public var ignoreVersion: Int64 {
        get { Int64(meta("ignore_version") ?? "") ?? -1 }
        set { try? setMeta("ignore_version", String(newValue)) }
    }

    public var ignoreRules: String {
        get { meta("ignore_rules") ?? "" }
        set { try? setMeta("ignore_rules", newValue) }
    }

    // MARK: - Entries

    /// What we last knew about a path.
    public func state(for path: String) throws -> SyncedState? {
        let statement = try prepare(
            "SELECT path, type, size, mtime, sha256, rev FROM state WHERE path = ?")
        defer { sqlite3_finalize(statement) }

        bind(statement, 1, path)
        guard sqlite3_step(statement) == SQLITE_ROW else { return nil }
        return row(statement)
    }

    /// Everything we last knew, keyed by path.
    public func allStates() throws -> [String: SyncedState] {
        let statement = try prepare("SELECT path, type, size, mtime, sha256, rev FROM state")
        defer { sqlite3_finalize(statement) }

        var result: [String: SyncedState] = [:]
        while sqlite3_step(statement) == SQLITE_ROW {
            let state = row(statement)
            result[state.path] = state
        }
        return result
    }

    public func count() throws -> Int {
        let statement = try prepare("SELECT COUNT(*) FROM state")
        defer { sqlite3_finalize(statement) }

        guard sqlite3_step(statement) == SQLITE_ROW else { return 0 }
        return Int(sqlite3_column_int64(statement, 0))
    }

    /// Records a path as synced.
    public func record(_ state: SyncedState) throws {
        let statement = try prepare("""
            INSERT INTO state (path, type, size, mtime, sha256, rev)
            VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT(path) DO UPDATE SET
                type = excluded.type, size = excluded.size, mtime = excluded.mtime,
                sha256 = excluded.sha256, rev = excluded.rev
            """)
        defer { sqlite3_finalize(statement) }

        bind(statement, 1, state.path)
        bind(statement, 2, state.type.rawValue)
        sqlite3_bind_int64(statement, 3, state.size)
        sqlite3_bind_int64(statement, 4, state.mtime)
        bind(statement, 5, state.sha256)
        sqlite3_bind_int64(statement, 6, state.rev)

        guard sqlite3_step(statement) == SQLITE_DONE else {
            throw LocalStateError.query(lastErrorMessage())
        }
    }

    /// Drops a path, because it no longer exists on either side.
    public func forget(_ path: String) throws {
        let statement = try prepare("DELETE FROM state WHERE path = ?")
        defer { sqlite3_finalize(statement) }

        bind(statement, 1, path)
        guard sqlite3_step(statement) == SQLITE_DONE else {
            throw LocalStateError.query(lastErrorMessage())
        }
    }

    /// Runs `body` in a transaction, so a batch of updates either all land or
    /// none do. A half-applied batch would leave the state lying about what is
    /// on disk, which is worse than not applying it at all.
    public func transaction<T>(_ body: () throws -> T) throws -> T {
        try execute("BEGIN IMMEDIATE")
        do {
            let result = try body()
            try execute("COMMIT")
            return result
        } catch {
            try? execute("ROLLBACK")
            throw error
        }
    }

    // MARK: - Plumbing

    private func execute(_ sql: String) throws {
        var errorMessage: UnsafeMutablePointer<CChar>?
        guard sqlite3_exec(db, sql, nil, nil, &errorMessage) == SQLITE_OK else {
            let detail = errorMessage.map { String(cString: $0) } ?? "unknown"
            sqlite3_free(errorMessage)
            throw LocalStateError.query("\(sql): \(detail)")
        }
    }

    private func prepare(_ sql: String) throws -> OpaquePointer? {
        var statement: OpaquePointer?
        guard sqlite3_prepare_v2(db, sql, -1, &statement, nil) == SQLITE_OK else {
            throw LocalStateError.query("\(sql): \(lastErrorMessage())")
        }
        return statement
    }

    private func bind(_ statement: OpaquePointer?, _ index: Int32, _ value: String) {
        sqlite3_bind_text(statement, index, value, -1, SQLITE_TRANSIENT)
    }

    private func row(_ statement: OpaquePointer?) -> SyncedState {
        SyncedState(
            path: column(statement, 0),
            type: EntryType(rawValue: column(statement, 1)) ?? .file,
            size: sqlite3_column_int64(statement, 2),
            mtime: sqlite3_column_int64(statement, 3),
            sha256: column(statement, 4),
            rev: sqlite3_column_int64(statement, 5)
        )
    }

    private func column(_ statement: OpaquePointer?, _ index: Int32) -> String {
        guard let text = sqlite3_column_text(statement, index) else { return "" }
        return String(cString: text)
    }

    private func meta(_ key: String) -> String? {
        guard let statement = try? prepare("SELECT value FROM meta WHERE key = ?") else { return nil }
        defer { sqlite3_finalize(statement) }

        bind(statement, 1, key)
        guard sqlite3_step(statement) == SQLITE_ROW else { return nil }
        return column(statement, 0)
    }

    private func setMeta(_ key: String, _ value: String) throws {
        let statement = try prepare("""
            INSERT INTO meta (key, value) VALUES (?, ?)
            ON CONFLICT(key) DO UPDATE SET value = excluded.value
            """)
        defer { sqlite3_finalize(statement) }

        bind(statement, 1, key)
        bind(statement, 2, value)

        guard sqlite3_step(statement) == SQLITE_DONE else {
            throw LocalStateError.query(lastErrorMessage())
        }
    }

    private func lastErrorMessage() -> String {
        guard let db else { return "no database" }
        return String(cString: sqlite3_errmsg(db))
    }
}
