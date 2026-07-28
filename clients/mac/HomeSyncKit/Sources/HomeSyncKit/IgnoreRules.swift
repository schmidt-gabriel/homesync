import Darwin
import Foundation

/// Decides what never leaves this machine.
///
/// Patterns are gitignore-shaped and matched against the path relative to the
/// sync root. The server's shared rules are applied *in addition to* the
/// platform noise list below, never instead of it: a rule someone removes on
/// another machine must not cause this one to start uploading `.DS_Store`.
public struct IgnoreRules: Sendable, Equatable {
    private struct Pattern: Sendable, Equatable {
        let glob: String
        /// `!pattern` re-includes something an earlier rule excluded.
        let isNegated: Bool
        /// A trailing `/` restricts the rule to directories.
        let directoriesOnly: Bool
        /// A pattern containing `/` matches the whole relative path; one
        /// without it matches any single component, at any depth.
        let matchesFullPath: Bool
    }

    private let patterns: [Pattern]

    /// Files macOS scatters through every directory, plus our own scratch
    /// files. Always applied, whatever the server says.
    public static let platformNoise: [String] = [
        ".DS_Store",
        "._*",
        // macOS writes this into any folder given a custom icon, including
        // ours. Without it, branding the sync folder would push a stray file
        // to every other machine.
        "Icon\r",
        ".Spotlight-V100",
        ".Trashes",
        ".fseventsd",
        ".TemporaryItems",
        ".DocumentRevisions-V100",
        ".apdisk",
        "*.swp",
        "~$*",
        "*.homesync-tmp",
    ]

    /// Just the platform noise, for a client that has not yet reached the
    /// server. Erring towards not uploading is the safe direction.
    public static let `default` = IgnoreRules(rules: "")

    /// Parses a rules document. Blank lines and `#` comments are skipped.
    public init(rules: String) {
        var parsed: [Pattern] = []

        for line in (Self.platformNoise + rules.split(separator: "\n", omittingEmptySubsequences: false).map(String.init)) {
            var text = line.trimmingCharacters(in: .whitespaces)

            guard !text.isEmpty, !text.hasPrefix("#") else { continue }

            var isNegated = false
            if text.hasPrefix("!") {
                isNegated = true
                text.removeFirst()
            }

            var directoriesOnly = false
            if text.hasSuffix("/") {
                directoriesOnly = true
                text.removeLast()
            }

            // A leading slash anchors to the root; it is not part of the glob.
            let anchored = text.hasPrefix("/")
            if anchored { text.removeFirst() }

            guard !text.isEmpty else { continue }

            parsed.append(Pattern(
                glob: text,
                isNegated: isNegated,
                directoriesOnly: directoriesOnly,
                matchesFullPath: anchored || text.contains("/")
            ))
        }

        patterns = parsed
    }

    /// Whether a path is excluded from syncing.
    ///
    /// Later rules win, so a negation can re-include something an earlier
    /// pattern excluded, exactly as in a `.gitignore`.
    public func excludes(_ path: String, isDirectory: Bool = false) -> Bool {
        // A file inside an ignored directory is ignored too. Without this, a
        // rule like `build/` would keep the directory out of the index while
        // its contents streamed up one by one.
        if !isDirectory {
            var prefix: [Substring] = []
            let components = path.split(separator: "/")
            for component in components.dropLast() {
                prefix.append(component)
                if excludes(prefix.joined(separator: "/"), isDirectory: true) {
                    return true
                }
            }
        }

        var excluded = false
        for pattern in patterns {
            if pattern.directoriesOnly && !isDirectory { continue }
            guard matches(pattern, path: path) else { continue }
            excluded = !pattern.isNegated
        }
        return excluded
    }

    private func matches(_ pattern: Pattern, path: String) -> Bool {
        if pattern.matchesFullPath {
            return Self.fnmatch(pattern.glob, path)
        }

        // A bare name applies to any component at any depth, which is what
        // makes `.DS_Store` cover every directory rather than only the root.
        for component in path.split(separator: "/") {
            if Self.fnmatch(pattern.glob, String(component)) {
                return true
            }
        }
        return false
    }

    /// Glob matching via `fnmatch(3)` rather than a hand-rolled matcher or a
    /// regex translation, both of which are easy to get subtly wrong.
    ///
    /// `**` is meant to cross directory boundaries, so `FNM_PATHNAME` (which
    /// stops `*` matching `/`) is applied only when the pattern does not use it.
    private static func fnmatch(_ pattern: String, _ candidate: String) -> Bool {
        let flags = pattern.contains("**") ? Int32(0) : Int32(FNM_PATHNAME)
        return Darwin.fnmatch(pattern, candidate, flags) == 0
    }
}
