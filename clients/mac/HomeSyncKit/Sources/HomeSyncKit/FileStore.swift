import CryptoKit
import Foundation

/// One path as it exists on this machine right now.
public struct LocalFile: Sendable, Equatable {
    public let path: String
    public let type: EntryType
    public let size: Int64
    /// Unix milliseconds, to match the server.
    public let mtime: Int64
}

public enum FileStoreError: Error, CustomStringConvertible {
    case outsideRoot(String)
    case notReadable(String, any Error)

    public var description: String {
        switch self {
        case .outsideRoot(let path): return "\(path) is outside the sync root"
        case .notReadable(let path, let error): return "cannot read \(path): \(error)"
        }
    }
}

/// Every read and write inside the sync folder. Nothing else touches the
/// filesystem, so the rules about atomicity and containment live in one place.
public struct FileStore: Sendable {
    /// The sync root.
    public let root: URL

    public init(root: URL) throws {
        try FileManager.default.createDirectory(at: root, withIntermediateDirectories: true)
        self.root = root.resolvingSymlinksInPath()
    }

    /// Puts a path into a single comparable form.
    ///
    /// macOS reaches `/var`, `/tmp` and `/etc` through *firmlinks*, which are
    /// not symlinks: neither `realpath(3)` nor `resolvingSymlinksInPath()`
    /// traverses them. So the very same directory is `/var/folders/…` when
    /// Foundation names it and `/private/var/folders/…` when FSEvents reports
    /// it, and a plain string comparison of the two never matches.
    ///
    /// Stripping the `/private` prefix from both sides is what makes them
    /// comparable. Without it a sync root under any of those three would
    /// silently watch nothing.
    static func canonical(_ path: String) -> String {
        guard path.hasPrefix("/private/") else { return path }
        return String(path.dropFirst("/private".count))
    }

    /// The absolute location of a sync path.
    public func url(for path: String) -> URL {
        root.appending(path: path, directoryHint: .notDirectory)
    }

    /// The sync path for an absolute location, or nil if it is outside the root.
    ///
    /// The result is normalised to NFC because the server's index is composed
    /// while macOS hands filenames back decomposed.
    public func relativePath(for url: URL) -> String? {
        let resolved = Self.canonical(url.resolvingSymlinksInPath().standardizedFileURL.path)
        let base = Self.canonical(root.standardizedFileURL.path)

        let rootComponents = base.split(separator: "/").map(String.init)
        let components = resolved.split(separator: "/").map(String.init)

        guard components.count > rootComponents.count,
              Array(components.prefix(rootComponents.count)) == rootComponents
        else { return nil }

        return components
            .dropFirst(rootComponents.count)
            .joined(separator: "/")
            .precomposedStringWithCanonicalMapping
    }

    // MARK: - Reading

    /// Walks the whole tree, skipping anything the rules exclude.
    public func scan(ignoring rules: IgnoreRules) throws -> [String: LocalFile] {
        let manager = FileManager.default
        guard let enumerator = manager.enumerator(
            at: root,
            includingPropertiesForKeys: [.isDirectoryKey, .isRegularFileKey, .isSymbolicLinkKey,
                                         .fileSizeKey, .contentModificationDateKey],
            options: []
        ) else {
            return [:]
        }

        var found: [String: LocalFile] = [:]

        for case let url as URL in enumerator {
            guard let path = relativePath(for: url) else { continue }

            let values = try? url.resourceValues(forKeys: [
                .isDirectoryKey, .isRegularFileKey, .isSymbolicLinkKey,
                .fileSizeKey, .contentModificationDateKey,
            ])
            let isDirectory = values?.isDirectory ?? false

            if rules.excludes(path, isDirectory: isDirectory) {
                if isDirectory { enumerator.skipDescendants() }
                continue
            }

            // Symlinks are out of scope for v1, matching the server.
            if values?.isSymbolicLink == true { continue }

            if isDirectory {
                found[path] = LocalFile(path: path, type: .dir, size: 0, mtime: modified(values))
                continue
            }
            guard values?.isRegularFile == true else { continue }

            found[path] = LocalFile(
                path: path,
                type: .file,
                size: Int64(values?.fileSize ?? 0),
                mtime: modified(values)
            )
        }

        return found
    }

    /// Describes a single path, or nil if it is absent.
    public func describe(_ path: String) -> LocalFile? {
        let url = url(for: path)
        guard let values = try? url.resourceValues(forKeys: [
            .isDirectoryKey, .isRegularFileKey, .isSymbolicLinkKey,
            .fileSizeKey, .contentModificationDateKey,
        ]) else { return nil }

        if values.isSymbolicLink == true { return nil }

        if values.isDirectory == true {
            return LocalFile(path: path, type: .dir, size: 0, mtime: modified(values))
        }
        guard values.isRegularFile == true else { return nil }

        return LocalFile(
            path: path,
            type: .file,
            size: Int64(values.fileSize ?? 0),
            mtime: modified(values)
        )
    }

    public func exists(_ path: String) -> Bool {
        FileManager.default.fileExists(atPath: url(for: path).path)
    }

    /// Copies a file to a temporary location and returns where it landed.
    ///
    /// Everything that follows — hashing, uploading, recording what was sent —
    /// must run against this copy, never the original. An editor writing the
    /// file while it is being read produces bytes that belong to no version of
    /// it: `URLSession.upload(fromFile:)` fixes the length when the request is
    /// made and pads the body if the file shrinks underneath it. Measured on a
    /// 4 MB file truncated to 5 bytes mid-upload, it still sent 4 MB, and the
    /// difference arrives as trailing NULs.
    ///
    /// The copy is cheap on APFS, which clones rather than duplicates blocks.
    /// The caller owns the result and must delete it.
    public func snapshot(_ path: String) throws -> URL {
        let source = url(for: path)
        let destination = FileManager.default.temporaryDirectory
            .appending(path: "homesync-upload-\(UUID().uuidString)")

        try FileManager.default.copyItem(at: source, to: destination)
        return destination
    }

    /// Streams a file at an absolute location through SHA-256.
    public func hash(contentsOf url: URL) throws -> String {
        do {
            let handle = try FileHandle(forReadingFrom: url)
            defer { try? handle.close() }

            var hasher = SHA256()
            while let chunk = try handle.read(upToCount: 1 << 20), !chunk.isEmpty {
                hasher.update(data: chunk)
            }
            return hasher.finalize().map { String(format: "%02x", $0) }.joined()
        } catch {
            throw FileStoreError.notReadable(url.path, error)
        }
    }

    /// Streams a file through SHA-256, so size is bounded by the buffer rather
    /// than by the file.
    public func hash(_ path: String) throws -> String {
        let url = url(for: path)
        do {
            let handle = try FileHandle(forReadingFrom: url)
            defer { try? handle.close() }

            var hasher = SHA256()
            while let chunk = try handle.read(upToCount: 1 << 20), !chunk.isEmpty {
                hasher.update(data: chunk)
            }
            return hasher.finalize().map { String(format: "%02x", $0) }.joined()
        } catch {
            throw FileStoreError.notReadable(path, error)
        }
    }

    // MARK: - Writing

    /// Moves a downloaded temporary file into place atomically.
    ///
    /// A reader therefore sees the whole old file or the whole new one, never a
    /// half-written one, and an interrupted sync leaves nothing broken behind.
    public func install(_ temporary: URL, at path: String, modified mtime: Int64?) throws {
        let destination = url(for: path)
        let manager = FileManager.default

        try manager.createDirectory(
            at: destination.deletingLastPathComponent(), withIntermediateDirectories: true)

        // replaceItemAt is the atomic swap; it needs a plain move when there is
        // nothing to replace.
        if manager.fileExists(atPath: destination.path) {
            _ = try manager.replaceItemAt(destination, withItemAt: temporary)
        } else {
            try manager.moveItem(at: temporary, to: destination)
        }

        // Carrying the server's mtime over means the next scan compares equal
        // and does not re-hash, let alone re-upload, what we just downloaded.
        if let mtime {
            let date = Date(timeIntervalSince1970: Double(mtime) / 1000)
            try? manager.setAttributes([.modificationDate: date], ofItemAtPath: destination.path)
        }
    }

    public func makeDirectory(_ path: String) throws {
        try FileManager.default.createDirectory(
            at: url(for: path), withIntermediateDirectories: true)
    }

    public func remove(_ path: String) throws {
        let url = url(for: path)
        guard FileManager.default.fileExists(atPath: url.path) else { return }
        try FileManager.default.removeItem(at: url)
    }

    /// Renames a path, used to park a local edit that lost a conflict.
    public func move(from source: String, to destination: String) throws {
        let target = url(for: destination)
        try FileManager.default.createDirectory(
            at: target.deletingLastPathComponent(), withIntermediateDirectories: true)
        try FileManager.default.moveItem(at: url(for: source), to: target)
    }

    private func modified(_ values: URLResourceValues?) -> Int64 {
        guard let date = values?.contentModificationDate else { return 0 }
        return Int64(date.timeIntervalSince1970 * 1000)
    }
}
