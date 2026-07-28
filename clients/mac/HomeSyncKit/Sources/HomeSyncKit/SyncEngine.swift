import Foundation

/// Keeps a local folder and a HomeSync server in step.
///
/// An `actor` because the sync state has exactly one owner and the compiler
/// should be the thing enforcing that, not a convention. Filesystem events, the
/// event stream and the poll timer all arrive concurrently; serialising them
/// here is what stops two cycles interleaving halfway through a change set.
public actor SyncEngine {
    private let configuration: Configuration
    private let api: APIClient
    private let store: FileStore
    private let state: LocalState

    private var rules: IgnoreRules
    private var isSyncing = false
    /// Set while a cycle is running and something else asks for one, so the
    /// request is honoured once rather than dropped or queued up N deep.
    private var syncRequestedAgain = false

    private var status: SyncState = .idle(lastSync: nil)
    private var knownConflicts: [String] = []

    public init(configuration: Configuration, session: URLSession = .shared) throws {
        self.configuration = configuration
        self.api = APIClient(
            baseURL: configuration.serverURL, token: configuration.token, session: session)
        self.store = try FileStore(root: configuration.root)
        self.state = try LocalState(at: configuration.stateURL)
        self.rules = IgnoreRules(rules: state.ignoreRules)
    }

    // MARK: - Observation

    public var currentState: SyncState { status }
    public var conflicts: [String] { knownConflicts }
    public var syncedFileCount: Int { (try? state.count()) ?? 0 }
    public var isTransportInsecure: Bool { api.isInsecure }

    public func clearConflicts() { knownConflicts.removeAll() }

    // MARK: - Running

    /// Runs until cancelled: watches the folder, listens for server events, and
    /// polls as a backstop.
    public func run() async {
        await refreshIgnoreRules()

        await withTaskGroup(of: Void.self) { group in
            group.addTask { [weak self] in await self?.watchLocalChanges() }
            group.addTask { [weak self] in await self?.watchServerEvents() }
            group.addTask { [weak self] in await self?.pollPeriodically() }
            await group.waitForAll()
        }
    }

    /// Reacts to local edits.
    private func watchLocalChanges() async {
        let watcher = FileWatcher(root: store.root)
        watcher.start()
        defer { watcher.stop() }

        // Sync once at startup, before any event arrives: whatever changed
        // while this client was not running has to be caught too.
        await requestSync()

        for await _ in watcher.changes {
            if Task.isCancelled { return }
            await requestSync()
        }
    }

    /// Reacts to other machines' changes.
    private func watchServerEvents() async {
        var backoff = Duration.seconds(1)

        while !Task.isCancelled {
            do {
                for try await _ in api.events() {
                    backoff = .seconds(1)
                    await requestSync()
                }
                // A clean end of stream is a disconnect, not a failure.
            } catch is CancellationError {
                return
            } catch {
                // Losing the stream never loses data: the next cycle asks for
                // everything since our own revision and catches up in one go.
                setStatusIfIdle(.failed("event stream: \(error)"))
            }

            if Task.isCancelled { return }
            try? await Task.sleep(for: backoff)
            backoff = min(backoff * 2, .seconds(60))
        }
    }

    /// The backstop for when the event stream is down.
    private func pollPeriodically() async {
        while !Task.isCancelled {
            try? await Task.sleep(for: configuration.pollInterval)
            if Task.isCancelled { return }
            await requestSync()
        }
    }

    /// Runs a cycle, coalescing overlapping requests into one follow-up.
    private func requestSync() async {
        guard !isSyncing else {
            syncRequestedAgain = true
            return
        }

        repeat {
            syncRequestedAgain = false
            do {
                _ = try await syncOnce()
            } catch {
                recordFailure(error)
            }
        } while syncRequestedAgain
    }

    // MARK: - One cycle

    /// Pulls, then pushes.
    ///
    /// The order matters: pulling first keeps the revisions we send as
    /// `X-Base-Rev` fresh, which turns what would have been a conflict into an
    /// ordinary update whenever the two edits did not really overlap in time.
    @discardableResult
    public func syncOnce() async throws -> SyncSummary {
        guard !isSyncing else { return SyncSummary() }
        isSyncing = true
        status = .syncing(progress: nil)
        defer { isSyncing = false }

        do {
            let pulled = try await pull()
            let pushed = try await push()

            let summary = pulled + pushed
            status = .idle(lastSync: Date())
            return summary
        } catch {
            recordFailure(error)
            throw error
        }
    }

    /// Reports a failure without discarding a more specific one.
    ///
    /// The delete guard pauses with the reason and the count, which is the only
    /// message the user can act on. Overwriting it with a generic error here
    /// would replace "refusing to delete 10 files, the volume is probably not
    /// mounted" with something they cannot do anything about.
    private func recordFailure(_ error: any Error) {
        if case .paused = status { return }
        status = .failed(String(describing: error))
    }

    // MARK: - Pull

    private func pull() async throws -> SyncSummary {
        var summary = SyncSummary()

        let (entries, currentRev) = try await api.allChanges(since: state.lastRev)
        guard !entries.isEmpty else {
            state.lastRev = currentRev
            return summary
        }

        try checkDeleteGuard(entries)

        let relevant = entries.filter { !rules.excludes($0.path, isDirectory: $0.type == .dir) }
        report(.downloading, completed: 0, total: relevant.count)

        for (index, entry) in relevant.enumerated() {
            report(.downloading, completed: index, total: relevant.count)

            do {
                try await apply(entry, into: &summary)
            } catch let error as SyncError {
                // One unreachable path must not stop the rest of the batch, but
                // we must not advance past it either: leaving lastRev where it
                // is means the next cycle retries.
                if case .notFound = error { continue }
                throw error
            }
        }

        state.lastRev = currentRev
        return summary
    }

    /// Refuses a pull that would delete an implausible share of the tree.
    ///
    /// A corrupted state database or an unmounted server volume both look, from
    /// here, exactly like "the user deleted everything". Pausing and telling
    /// someone is the only safe response.
    private func checkDeleteGuard(_ entries: [RemoteEntry]) throws {
        let deletions = entries.filter { $0.deleted && store.exists($0.path) }.count
        guard deletions > 0 else { return }

        let known = max((try? state.count()) ?? 0, 1)
        let fractionLimit = Int(Double(known) * configuration.maxDeleteFraction)
        let limit = max(min(configuration.maxDeletesPerPull, fractionLimit), 1)

        guard deletions > limit else { return }

        let reason = """
            refusing to delete \(deletions) local files in one go (limit \(limit)). \
            This usually means the server's volume is not mounted or the local \
            state is damaged, not that the files were really deleted.
            """
        status = .paused(reason: reason)
        throw SyncError.invalidResponse(reason)
    }

    private func apply(_ entry: RemoteEntry, into summary: inout SyncSummary) async throws {
        // Our own write, coming back to us.
        //
        // Pushing advances the server's revision past the cursor we pulled
        // from, so the next pull always re-reads what we just uploaded. If that
        // is treated as a remote change, the client ends up fighting itself:
        // a local edit made after the upload looks like both sides having
        // changed, and gets parked as a bogus conflict — and a local deletion
        // gets undone by the file being downloaded again before the push phase
        // ever gets to delete it.
        //
        // Recognising the echo by revision *and* content is what settles it:
        // if both match what we recorded when we pushed, the entry carries
        // nothing we do not already know.
        if !entry.deleted,
           let recorded = try state.state(for: entry.path),
           recorded.rev == entry.rev,
           recorded.sha256 == (entry.sha256 ?? recorded.sha256) {
            return
        }

        if entry.deleted {
            try applyDeletion(entry, into: &summary)
            return
        }

        switch entry.type {
        case .dir:
            if !store.exists(entry.path) {
                try store.makeDirectory(entry.path)
                summary.directoriesCreated += 1
            }
            try state.record(SyncedState(
                path: entry.path, type: .dir, size: 0,
                mtime: entry.mtime, sha256: "", rev: entry.rev))

        case .file:
            try await applyFile(entry, into: &summary)
        }
    }

    private func applyDeletion(_ entry: RemoteEntry, into summary: inout SyncSummary) throws {
        guard let local = store.describe(entry.path) else {
            try state.forget(entry.path)
            return
        }

        // The server says this is gone, but it changed here since we last
        // agreed. Deleting would silently discard that edit, so keep the file:
        // the push phase will upload it again as a new path.
        if local.type == .file, let recorded = try state.state(for: entry.path) {
            let unchanged = local.size == recorded.size && local.mtime == recorded.mtime
            if !unchanged, (try? store.hash(entry.path)) != recorded.sha256 {
                try state.forget(entry.path)
                return
            }
        }

        try store.remove(entry.path)
        try state.forget(entry.path)
        summary.deletedLocally += 1
    }

    private func applyFile(_ entry: RemoteEntry, into summary: inout SyncSummary) async throws {
        let recorded = try state.state(for: entry.path)
        let local = store.describe(entry.path)

        // Already identical: record where it now stands and move on without
        // spending a request.
        if let local, local.type == .file, let remoteSHA = entry.sha256 {
            let localSHA = try? store.hash(entry.path)
            if localSHA == remoteSHA {
                try state.record(SyncedState(
                    path: entry.path, type: .file, size: local.size,
                    mtime: local.mtime, sha256: remoteSHA, rev: entry.rev))
                return
            }
        }

        // Both sides moved. Park the local version under a name that says where
        // it came from, then take the server's. Nothing is lost: the copy is
        // uploaded on the next push and appears on every machine.
        if let local, local.type == .file, let recorded {
            let localSHA = (try? store.hash(entry.path)) ?? ""
            if localSHA != recorded.sha256 {
                let parked = Self.conflictName(
                    for: entry.path, device: configuration.deviceName, at: Date())
                try store.move(from: entry.path, to: parked)
                try state.forget(entry.path)
                summary.conflicts.append(parked)
                noteConflict(parked)
            }
        }

        let download = try await api.download(path: entry.path)
        try store.install(download.file, at: entry.path, modified: entry.mtime)

        // Read back what actually landed rather than assuming: if setting the
        // modification date failed, recording the intended value would make
        // every later scan think the file had changed and re-upload it.
        let installed = store.describe(entry.path)
        try state.record(SyncedState(
            path: entry.path,
            type: .file,
            size: installed?.size ?? entry.size,
            mtime: installed?.mtime ?? entry.mtime,
            sha256: entry.sha256 ?? download.sha256 ?? "",
            rev: entry.rev
        ))
        summary.downloaded += 1
    }

    // MARK: - Push

    private func push() async throws -> SyncSummary {
        var summary = SyncSummary()

        let onDisk = try store.scan(ignoring: rules)
        let recorded = try state.allStates()

        // Directories first, so an empty one survives the round trip. A
        // directory with files in it is created implicitly by its contents.
        for (path, file) in onDisk where file.type == .dir {
            guard recorded[path] == nil else { continue }
            try await api.createDirectory(path: path)
            try state.record(SyncedState(
                path: path, type: .dir, size: 0, mtime: file.mtime, sha256: "", rev: 0))
            summary.directoriesCreated += 1
        }

        // Counted before uploading rather than as we go, so the total does not
        // climb while the bar is moving.
        let pending = onDisk.values.filter { file in
            guard file.type == .file else { return false }
            guard let known = recorded[file.path] else { return true }
            return known.size != file.size || known.mtime != file.mtime
        }.count
        let removals = recorded.keys.filter { onDisk[$0] == nil }.count
        var done = 0
        report(.uploading, completed: 0, total: pending + removals)

        for (path, file) in onDisk where file.type == .file {
            let known = recorded[path]

            // Cheap check first. Hashing every file on every cycle would make
            // the folder unusable at any real size; size and mtime are a good
            // enough filter to decide what is worth hashing.
            if let known, known.size == file.size, known.mtime == file.mtime {
                continue
            }

            // Snapshot before doing anything else with it. A file being saved
            // by an editor changes underneath a reader, and hashing one version
            // while uploading another is how corruption gets in. Everything
            // below describes this copy, which cannot change.
            let snapshot: URL
            do {
                snapshot = try store.snapshot(path)
            } catch {
                // Vanished between the scan and now. The next cycle will see
                // it gone and handle it as a deletion.
                continue
            }
            defer { try? FileManager.default.removeItem(at: snapshot) }

            let sha = try store.hash(contentsOf: snapshot)

            // Content identical despite a touched mtime. Record the new mtime
            // so we stop re-hashing it, but do not upload: a stray `touch`
            // must not wake every other machine.
            if let known, known.sha256 == sha {
                try state.record(SyncedState(
                    path: path, type: .file, size: file.size,
                    mtime: file.mtime, sha256: sha, rev: known.rev))
                continue
            }

            // The size recorded has to be the snapshot's, not the one the scan
            // saw: if the file grew or shrank in between, the scan's figure
            // describes bytes nobody uploaded.
            let snapshotSize = (try? FileManager.default
                .attributesOfItem(atPath: snapshot.path)[.size] as? Int64) ?? file.size
            let sent = LocalFile(
                path: path, type: .file, size: snapshotSize ?? file.size, mtime: file.mtime)

            try await upload(
                path, file: sent, from: snapshot, sha: sha,
                baseRev: known?.rev ?? 0, into: &summary)
            done += 1
            report(.uploading, completed: done, total: pending + removals)
        }

        try await pushDeletions(onDisk: onDisk, recorded: recorded, into: &summary)
        return summary
    }

    private func upload(
        _ path: String, file: LocalFile, from source: URL, sha: String, baseRev: Int64,
        into summary: inout SyncSummary
    ) async throws {
        do {
            let response = try await api.upload(
                path: path, from: source, baseRev: baseRev, sha256: sha)
            try state.record(SyncedState(
                path: path, type: .file, size: file.size,
                mtime: file.mtime, sha256: sha, rev: response.rev))
            summary.uploaded += 1

        } catch let error as SyncError {
            guard case .conflict(_, let conflict, _) = error else { throw error }

            // The server kept its version and stored ours under `conflict`.
            // Take its version now so both exist here too; ours comes back
            // down on the next pull under the new name.
            if let conflict {
                summary.conflicts.append(conflict)
                noteConflict(conflict)
            }

            let download = try await api.download(path: path)
            try store.install(download.file, at: path, modified: nil)

            let installed = store.describe(path)
            try state.record(SyncedState(
                path: path, type: .file,
                size: installed?.size ?? 0, mtime: installed?.mtime ?? 0,
                sha256: download.sha256 ?? (try? store.hash(path)) ?? "",
                rev: download.rev))
            summary.downloaded += 1
        }
    }

    /// Publishes progress, but only when there is enough work for a number to
    /// mean anything. A cycle of three files would flash a percentage that is
    /// gone before it can be read.
    private func report(_ phase: SyncProgress.Phase, completed: Int, total: Int) {
        guard total >= Self.progressThreshold else {
            status = .syncing(progress: nil)
            return
        }
        status = .syncing(progress: SyncProgress(phase: phase, completed: completed, total: total))
    }

    private static let progressThreshold = 12

    private func pushDeletions(
        onDisk: [String: LocalFile], recorded: [String: SyncedState],
        into summary: inout SyncSummary
    ) async throws {
        for (path, known) in recorded where onDisk[path] == nil {
            // A path we have never uploaded cannot be deleted remotely; drop it
            // from our record and move on.
            guard known.rev > 0 else {
                try state.forget(path)
                continue
            }

            do {
                try await api.delete(path: path, baseRev: known.rev)
                try state.forget(path)
                summary.deletedRemotely += 1
            } catch let error as SyncError {
                switch error {
                case .notFound:
                    // Someone else got there first.
                    try state.forget(path)
                case .stale:
                    // It changed on the server after we last saw it. Deleting
                    // would discard that edit, so leave it: the next pull
                    // brings the newer version back down.
                    try state.forget(path)
                case .server(_, "not_empty", _):
                    // Its children go first; the next cycle will get it.
                    continue
                default:
                    throw error
                }
            }
        }
    }

    // MARK: - Ignore rules

    /// Fetches the shared rules, so a pattern added on one machine takes effect
    /// everywhere.
    public func refreshIgnoreRules() async {
        guard let document = try? await api.ignoreRules() else { return }
        guard document.version != state.ignoreVersion else { return }

        state.ignoreRules = document.rules
        state.ignoreVersion = document.version
        rules = IgnoreRules(rules: document.rules)
    }

    public func setIgnoreRules(_ text: String) async throws {
        try await api.setIgnoreRules(text)
        state.ignoreRules = text
        rules = IgnoreRules(rules: text)
        if let document = try? await api.ignoreRules() {
            state.ignoreVersion = document.version
        }
    }

    public var ignoreRulesText: String { state.ignoreRules }

    // MARK: - Helpers

    private func noteConflict(_ path: String) {
        guard !knownConflicts.contains(path) else { return }
        knownConflicts.append(path)
    }

    private func setStatusIfIdle(_ new: SyncState) {
        if case .syncing = status { return }
        status = new
    }

    /// `<stem>.conflict-<device>-<yyyyMMdd-HHmmss><ext>`, matching what the
    /// server generates, so a copy made here is indistinguishable from one made
    /// there.
    static func conflictName(for path: String, device: String, at date: Date) -> String {
        let formatter = DateFormatter()
        formatter.dateFormat = "yyyyMMdd-HHmmss"
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = .current

        let url = URL(fileURLWithPath: path)
        let ext = url.pathExtension
        let stem = url.deletingPathExtension().lastPathComponent
        let directory = (path as NSString).deletingLastPathComponent

        var name = "\(stem).conflict-\(sanitise(device))-\(formatter.string(from: date))"
        if !ext.isEmpty { name += ".\(ext)" }

        return directory.isEmpty ? name : "\(directory)/\(name)"
    }

    private static func sanitise(_ device: String) -> String {
        let allowed = device.map { character -> Character in
            if character.isLetter || character.isNumber || character == "-" { return character }
            return "-"
        }
        let cleaned = String(allowed)
            .split(separator: "-", omittingEmptySubsequences: true)
            .joined(separator: "-")

        return cleaned.isEmpty ? "unknown" : String(cleaned.prefix(32))
    }
}
