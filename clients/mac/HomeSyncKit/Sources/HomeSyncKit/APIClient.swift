import Foundation

/// Talks to a HomeSync server. Stateless apart from its configuration, so it
/// is safe to share.
public struct APIClient: Sendable {
    private let baseURL: URL
    private let token: String
    private let session: URLSession

    public init(baseURL: URL, token: String, session: URLSession = .shared) {
        self.baseURL = baseURL
        self.token = token
        self.session = session
    }

    /// True when the token is not protected in transit. The bearer token grants
    /// full read and write access, so a client should say so plainly.
    public var isInsecure: Bool {
        baseURL.scheme?.lowercased() != "https"
    }

    // MARK: - URL construction

    /// Builds a URL for a sync path.
    ///
    /// Two things happen here that must not be skipped. The path is normalised
    /// to NFC, because macOS hands out decomposed filenames and the server's
    /// index is composed. And it is percent-encoded with a set that keeps `/`
    /// as a separator while escaping everything else, so a filename containing
    /// `?` or `#` cannot alter the shape of the request.
    private func url(forEndpoint endpoint: String, path: String) -> URL? {
        let normalised = path.precomposedStringWithCanonicalMapping
        guard let encoded = normalised.addingPercentEncoding(
            withAllowedCharacters: .urlPathAllowed
        ) else { return nil }

        return URL(string: "\(baseURL.absoluteString)/v1/\(endpoint)/\(encoded)")
    }

    private func request(_ method: String, url: URL) -> URLRequest {
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        return request
    }

    // MARK: - Changes

    /// Fetches one page of changes after `rev`.
    public func changes(since rev: Int64, limit: Int = 1000) async throws -> ChangesPage {
        guard var components = URLComponents(
            url: baseURL.appendingPathComponent("v1/changes"), resolvingAgainstBaseURL: false
        ) else {
            throw SyncError.invalidResponse("cannot build a changes URL from \(baseURL)")
        }
        components.queryItems = [
            URLQueryItem(name: "since", value: String(rev)),
            URLQueryItem(name: "limit", value: String(limit)),
        ]
        guard let url = components.url else {
            throw SyncError.invalidResponse("cannot build a changes URL")
        }

        let (data, response) = try await perform(request("GET", url: url))
        try check(response, data: data, path: "changes")

        do {
            return try JSONDecoder().decode(ChangesPage.self, from: data)
        } catch {
            throw SyncError.invalidResponse("changes: \(error)")
        }
    }

    /// Fetches every change after `rev`, following pagination to the end.
    public func allChanges(since rev: Int64) async throws -> (entries: [RemoteEntry], currentRev: Int64) {
        var collected: [RemoteEntry] = []
        var cursor = rev

        while true {
            let page = try await changes(since: cursor)
            collected.append(contentsOf: page.changes)

            guard page.more, let last = page.changes.last else {
                return (collected, page.currentRev)
            }

            // Guard against a server that sets `more` without advancing: that
            // would otherwise be an infinite loop holding the sync open.
            guard last.rev > cursor else {
                return (collected, page.currentRev)
            }
            cursor = last.rev
        }
    }

    // MARK: - Files

    /// A downloaded file, still in its temporary location.
    public struct Download: Sendable {
        public let file: URL
        /// The revision the content was current at. Carrying it back means a
        /// caller that had to re-fetch a path, after losing a conflict, can
        /// record where it now stands without a second request.
        public let rev: Int64
        public let sha256: String?
    }

    /// Downloads a path to a temporary file.
    ///
    /// The caller is responsible for moving or deleting it. Downloading to a
    /// temporary file rather than into memory keeps a large file from being
    /// held whole, and is what makes the eventual write atomic.
    public func download(path: String) async throws -> Download {
        guard let url = url(forEndpoint: "files", path: path) else {
            throw SyncError.invalidResponse("cannot build a URL for \(path)")
        }

        let temporary: URL
        let response: URLResponse
        do {
            (temporary, response) = try await session.download(for: request("GET", url: url))
        } catch {
            throw SyncError.transport(error)
        }

        guard let http = response as? HTTPURLResponse else {
            throw SyncError.invalidResponse("not an HTTP response")
        }
        guard http.statusCode == 200 else {
            // The body of a failed download is on disk, not in memory.
            let data = (try? Data(contentsOf: temporary)) ?? Data()
            try? FileManager.default.removeItem(at: temporary)
            try check(http, data: data, path: path)
            throw SyncError.invalidResponse("download of \(path) failed with \(http.statusCode)")
        }

        let rev = Int64(http.value(forHTTPHeaderField: "X-Base-Rev") ?? "") ?? 0
        let sha = http.value(forHTTPHeaderField: "ETag")?
            .trimmingCharacters(in: CharacterSet(charactersIn: "\"W/"))

        return Download(file: temporary, rev: rev, sha256: sha)
    }

    /// Uploads a file, declaring the revision believed to be current.
    ///
    /// Throws `.conflict` when the server parked the content elsewhere because
    /// the path had moved on. That is not a failure to retry: it means both
    /// versions now exist and the next pull will bring them down.
    @discardableResult
    public func upload(path: String, from file: URL, baseRev: Int64) async throws -> FileResponse {
        guard let url = url(forEndpoint: "files", path: path) else {
            throw SyncError.invalidResponse("cannot build a URL for \(path)")
        }

        var request = self.request("PUT", url: url)
        request.setValue(String(baseRev), forHTTPHeaderField: "X-Base-Rev")
        request.setValue("application/octet-stream", forHTTPHeaderField: "Content-Type")

        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await session.upload(for: request, fromFile: file)
        } catch {
            throw SyncError.transport(error)
        }

        try check(response, data: data, path: path)

        do {
            return try JSONDecoder().decode(FileResponse.self, from: data)
        } catch {
            throw SyncError.invalidResponse("upload of \(path): \(error)")
        }
    }

    /// Deletes a path at a known revision.
    @discardableResult
    public func delete(path: String, baseRev: Int64) async throws -> FileResponse {
        guard let url = url(forEndpoint: "files", path: path) else {
            throw SyncError.invalidResponse("cannot build a URL for \(path)")
        }

        var request = self.request("DELETE", url: url)
        request.setValue(String(baseRev), forHTTPHeaderField: "X-Base-Rev")

        let (data, response) = try await perform(request)
        try check(response, data: data, path: path)

        do {
            return try JSONDecoder().decode(FileResponse.self, from: data)
        } catch {
            throw SyncError.invalidResponse("delete of \(path): \(error)")
        }
    }

    /// Creates a directory, including any missing parents.
    public func createDirectory(path: String) async throws {
        guard let url = url(forEndpoint: "dirs", path: path) else {
            throw SyncError.invalidResponse("cannot build a URL for \(path)")
        }

        let (data, response) = try await perform(request("PUT", url: url))
        try check(response, data: data, path: path)
    }

    // MARK: - Ignore rules

    public func ignoreRules() async throws -> IgnoreDocument {
        let url = baseURL.appendingPathComponent("v1/ignore")
        let (data, response) = try await perform(request("GET", url: url))
        try check(response, data: data, path: "ignore")

        do {
            return try JSONDecoder().decode(IgnoreDocument.self, from: data)
        } catch {
            throw SyncError.invalidResponse("ignore: \(error)")
        }
    }

    public func setIgnoreRules(_ rules: String) async throws {
        let url = baseURL.appendingPathComponent("v1/ignore")
        var request = self.request("PUT", url: url)
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.httpBody = try JSONEncoder().encode(["rules": rules])

        let (data, response) = try await perform(request)
        try check(response, data: data, path: "ignore")
    }

    // MARK: - Events

    /// An async sequence of revision numbers announced by the server.
    ///
    /// The payload is only a number: the caller always follows up with
    /// `changes(since:)`. That is what makes a dropped, delayed or coalesced
    /// event harmless, and it is why this sequence ending is not an error.
    public func events() -> AsyncThrowingStream<Int64, any Error> {
        AsyncThrowingStream { continuation in
            let task = Task {
                do {
                    let url = baseURL.appendingPathComponent("v1/events")
                    var request = self.request("GET", url: url)
                    request.timeoutInterval = .infinity
                    request.setValue("text/event-stream", forHTTPHeaderField: "Accept")

                    let (bytes, response) = try await session.bytes(for: request)

                    guard let http = response as? HTTPURLResponse else {
                        throw SyncError.invalidResponse("not an HTTP response")
                    }
                    guard http.statusCode == 200 else {
                        if http.statusCode == 401 { throw SyncError.unauthorized }
                        throw SyncError.server(
                            status: http.statusCode, code: "events",
                            message: "the event stream was refused")
                    }

                    for try await line in bytes.lines {
                        // Comment lines are the server's heartbeat.
                        guard line.hasPrefix("data: ") else { continue }

                        let payload = String(line.dropFirst("data: ".count))
                        guard
                            let data = payload.data(using: .utf8),
                            let object = try? JSONDecoder().decode([String: Int64].self, from: data),
                            let rev = object["rev"]
                        else { continue }

                        continuation.yield(rev)
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }

            continuation.onTermination = { _ in task.cancel() }
        }
    }

    // MARK: - Plumbing

    private func perform(_ request: URLRequest) async throws -> (Data, URLResponse) {
        do {
            return try await session.data(for: request)
        } catch {
            throw SyncError.transport(error)
        }
    }

    /// Turns a non-2xx response into the most specific error available, so
    /// callers can branch on meaning rather than on status codes.
    private func check(_ response: URLResponse, data: Data, path: String) throws {
        guard let http = response as? HTTPURLResponse else {
            throw SyncError.invalidResponse("not an HTTP response")
        }
        guard !(200..<300).contains(http.statusCode) else { return }

        let details = try? JSONDecoder().decode(ServerError.self, from: data)
        let code = details?.error ?? "unknown"
        let message = details?.message ?? "no message"

        switch (http.statusCode, code) {
        case (401, _):
            throw SyncError.unauthorized
        case (404, _):
            throw SyncError.notFound(path: path)
        case (409, "conflict"):
            throw SyncError.conflict(path: path, conflict: details?.conflict, message: message)
        case (409, "stale"):
            throw SyncError.stale(path: path, message: message)
        default:
            throw SyncError.server(status: http.statusCode, code: code, message: message)
        }
    }
}
