import CoreServices
import Foundation

/// Watches the sync folder and reports when something under it changes.
///
/// FSEvents is the reason this client is native. It watches a whole tree
/// recursively for the cost of one stream, which nothing available to a script
/// or a launchd job can do: `WatchPaths` in a LaunchAgent only fires for the
/// directory node itself, so a file saved three levels down would go unnoticed
/// until the next poll.
public final class FileWatcher {
    private let root: URL
    private let latency: TimeInterval
    private var stream: FSEventStreamRef?
    private let queue: DispatchQueue

    /// Emits after each burst of filesystem activity. The payload is deliberately
    /// empty: FSEvents coalesces, and can report a directory rather than the
    /// file inside it, so the only honest reading of an event is "something
    /// moved, go and look".
    private let continuation: AsyncStream<Void>.Continuation
    public let changes: AsyncStream<Void>

    /// - Parameter latency: how long FSEvents batches events before delivering.
    ///   A saved file typically produces a burst of writes, and reacting to each
    ///   one separately would mean hashing the same file repeatedly.
    public init(root: URL, latency: TimeInterval = 0.5) {
        self.root = root.resolvingSymlinksInPath()
        self.latency = latency
        self.queue = DispatchQueue(label: "dev.schmidt.HomeSync.watcher")

        var escapee: AsyncStream<Void>.Continuation!
        self.changes = AsyncStream { escapee = $0 }
        self.continuation = escapee
    }

    deinit {
        stop()
    }

    public func start() {
        guard stream == nil else { return }

        // The C callback cannot capture context, so the object is handed
        // through FSEvents as an unretained pointer. Unretained is safe here
        // because stop() runs in deinit, before the object can go away.
        var context = FSEventStreamContext(
            version: 0,
            info: Unmanaged.passUnretained(self).toOpaque(),
            retain: nil,
            release: nil,
            copyDescription: nil
        )

        let callback: FSEventStreamCallback = { _, info, _, _, _, _ in
            guard let info else { return }
            let watcher = Unmanaged<FileWatcher>.fromOpaque(info).takeUnretainedValue()
            watcher.continuation.yield(())
        }

        guard let stream = FSEventStreamCreate(
            kCFAllocatorDefault,
            callback,
            &context,
            [root.path] as CFArray,
            FSEventStreamEventId(kFSEventStreamEventIdSinceNow),
            latency,
            // FileEvents reports individual files rather than directories;
            // WatchRoot keeps working if the folder itself is moved or renamed.
            FSEventStreamCreateFlags(
                kFSEventStreamCreateFlagFileEvents |
                kFSEventStreamCreateFlagWatchRoot |
                kFSEventStreamCreateFlagNoDefer
            )
        ) else {
            return
        }

        FSEventStreamSetDispatchQueue(stream, queue)
        FSEventStreamStart(stream)
        self.stream = stream
    }

    public func stop() {
        guard let stream else { return }
        FSEventStreamStop(stream)
        FSEventStreamInvalidate(stream)
        FSEventStreamRelease(stream)
        self.stream = nil
        continuation.finish()
    }
}
