# HomeSync protocol v1

This document is the contract. The Go server and the macOS client are two
implementations of it, not the specification itself. Anyone writing a third
client should be able to work from this file alone, and should prove the result
against the `conformance/` suite rather than against another client's
behaviour.

Everything here is stable within `/v1`. A breaking change means `/v2`.

---

## 1. The model

The server owns the files and an **index** describing them. Every mutation
increments a single, server-wide **revision** counter, and the affected path
records the revision at which it changed.

A client therefore stores exactly one number: the last revision it has fully
processed. To catch up it asks "what changed after revision N?". This is the
whole reason the multi-machine case is simple: no client tracks any other
client, and adding a fifth machine costs nothing.

Three consequences worth internalising before implementing anything:

- **Deletions are entries, not absences.** A deleted path stays in the index as
  a tombstone (`"deleted": true`). A client that was offline learns what
  disappeared the same way it learns what appeared.
- **Revisions are global, not per file.** They are gapless and strictly
  increasing across the whole tree. Do not assume a path's revisions are
  contiguous.
- **The server never merges.** When two machines edit from the same base, the
  loser's content is stored beside the winner's under a generated name. See
  §7.

---

## 2. Transport and authentication

Base URL is whatever the server is published at, for example
`http://homelab.local:8420`. All paths below are relative to it.

Every endpoint except `GET /healthz` requires:

```
Authorization: Bearer <device-token>
```

Tokens are minted per device (`homesync device add <name>`, or the admin UI).
The server stores only a SHA-256 of the token, so a token is displayed once and
cannot be recovered later. Revoking a device invalidates its token immediately.

A missing, malformed or unknown token gets `401` with a
`WWW-Authenticate: Bearer realm="homesync"` header.

> The token is a bearer credential: anyone holding it has full read and write
> access to the data. Over plain HTTP it is visible to anything on the path.
> Use TLS (`TLS_CERT`/`TLS_KEY`, or a reverse proxy) for anything that is not a
> trusted local network. A client should warn when its configured URL is not
> `https`.

### Encryption at rest is invisible here

A server may encrypt file contents on its own volume. Nothing in this protocol
changes when it does, and a client cannot tell: bodies are sent and received as
plaintext, and `size` and `sha256` always describe that plaintext, never the
bytes on the server's disk.

Do not implement anything for it. A client that tried to account for the
server's storage would be wrong on a server that changed its mind.

---

## 3. Scopes

Each device syncs one subtree of the server's data directory, its **scope**,
which defaults to a folder named after the device.

This is invisible to a client. Every path it sends is resolved inside its scope
and every path it receives has the scope stripped off, so a client always
believes it is at the root. A client implementation needs no code for this at
all.

What it buys is that sharing becomes a decision rather than the only option:

```
/data/MacBook/     device "MacBook"
/data/iMac/        device "iMac"
/data/Shared/      devices "MacBook-shared" and "iMac-shared"
```

Two devices pointed at the same scope sync the same files, exactly as before
scopes existed. Pointed at different ones, they never see each other.

The scope directory itself is never an entry in `/v1/changes` — it is the
device's root, and a client has no entry for its own root. It is also excluded
before pagination, not filtered out afterwards, so a page is never empty while
`more` is true.

A device whose scope changes must resync from `since=0`: the revisions it
remembers describe a tree it can no longer see.

Scopes are administrative. There is no endpoint for a device to read or change
its own; that happens through the CLI or the admin UI.

## 4. Paths

Paths appear in URLs after `/v1/files/` or `/v1/dirs/`, and in JSON as the
`path` field. They are always **relative to the device's scope**,
slash-separated, with no leading slash: `projects/alpha/notes.md`.

Rules the server enforces, and which a client must mirror:

**Unicode normalisation.** All paths are stored and returned in **NFC**. The
server normalises whatever it receives. This matters because macOS hands out
filenames in NFD (an `ç` arrives as `c` plus a combining cedilla) while Linux
and Windows use NFC; without a single canonical form the same file uploaded
from two platforms would occupy two index entries. A client reads and writes
its local filesystem in whatever form that filesystem uses, and converts at the
boundary.

**No dot segments.** A path containing a `.` or `..` segment is rejected with
`400 invalid_path`, in both raw and percent-encoded form. The server does not
redirect to a cleaned location.

**Case collisions are refused.** If a live path already exists that differs
only by letter case, the request fails with `409 case_collision` and nothing is
written. This is not fussiness: APFS and NTFS are case-insensitive by default,
so accepting both `Notas.md` and `notas.md` would silently collapse them into
one file and destroy one of them.

**Windows-hostile names are stored but flagged.** Names containing `<>:"|?*`, or
ending in a space or dot, or whose stem is a reserved device name (`CON`,
`NUL`, `AUX`, `COM1`…) are accepted and carry `"unsafe": true` in the index.
A macOS-only setup has every right to use them. A client on a filesystem that
cannot represent the name should escape it locally rather than fail silently.

**Symlinks are out of scope in v1.** The server skips them when scanning and
refuses to follow them when reading. Clients should skip them too.

---

## 5. Common shapes

### Entry

Returned by `/v1/changes`. One path at one revision.

```json
{
  "path": "projects/alpha/notes.md",
  "type": "file",
  "size": 1284,
  "mtime": 1785194027083,
  "sha256": "27eb5e51506c911f6fc4bb345c0d9db6f60415fceab7c18e1e9b862637415777",
  "rev": 42,
  "deleted": false,
  "unsafe": false
}
```

| Field | Notes |
|---|---|
| `type` | `"file"` or `"dir"` |
| `size` | Bytes. `0` for directories and tombstones |
| `mtime` | **Unix milliseconds**, not seconds |
| `sha256` | Hex, lowercase. Absent for directories and tombstones |
| `rev` | Revision at which this path last changed |
| `deleted` | `true` means tombstone: the path is gone |
| `unsafe` | Present only when `true` (see §4) |

### Error

Every failure uses one shape, so a client parses errors once:

```json
{ "error": "case_collision", "message": "case collision: \"notas.md\" already exists" }
```

`error` is a stable machine-readable code; `message` is for humans and logs and
may change. The full set of codes:

| Code | Status | Meaning |
|---|---|---|
| `unauthorized` | 401 | Missing or unknown bearer token |
| `invalid_path` | 400 | Dot segment, empty, or escapes the root |
| `invalid_base_rev` | 400 | `X-Base-Rev` is not a non-negative integer |
| `invalid_since` / `invalid_limit` | 400 | Bad query parameter |
| `invalid_body` | 400 | Malformed JSON body |
| `not_found` | 404 | No such path, or no such trash item |
| `is_directory` | 400/409 | Path is a directory, a file was expected |
| `is_file` | 409 | Path is a file, a directory was expected |
| `conflict` | 409 | Concurrent edit; body stored separately (§7) |
| `stale` | 409 | `X-Base-Rev` does not match; refetch first |
| `case_collision` | 409 | Differs from an existing path only by case |
| `not_empty` | 409 | Directory still has indexed children |
| `not_regular` | 409 | Target exists but is not a regular file |
| `hash_mismatch` | 422 | Body did not match `X-Content-SHA256`; nothing was stored |
| `occupied` | 409 | Restore target already exists |
| `internal` | 500 | Server-side failure |

---

## 6. Endpoints

### `GET /healthz`

No authentication. For container healthchecks, and for asking a server which
build it is running without holding a token for it.

```json
{ "status": "ok", "rev": 42, "version": "1.1.2" }
```

`version` is the release the binary was built from. A server built from a
working copy rather than a tag reports `"local"`.

### `GET /v1/changes?since=<rev>&limit=<n>`

The heart of the protocol.

- `since` (default `0`): return entries with `rev` strictly greater than this.
  `since=0` returns the entire index, including tombstones.
- `limit` (default `1000`, max `10000`).

```json
{
  "changes": [ /* Entry, ordered by rev ascending */ ],
  "current_rev": 87,
  "more": false
}
```

`more: true` means the page was truncated. Call again with `since` set to the
`rev` of the last entry you received. When `more` is `false`, you are current as
of `current_rev`; store that number.

Entries are ordered by revision, so applying them in order is always safe.

### `GET /v1/files/{path}` · `HEAD /v1/files/{path}`

Returns the raw bytes.

| Response header | Value |
|---|---|
| `Content-Type` | `application/octet-stream` |
| `ETag` | The sha256 in quotes |
| `X-Base-Rev` | The path's current revision |

`Range` and conditional requests (`If-None-Match`, `If-Modified-Since`) are
supported, so a large interrupted download can be resumed.

`404 not_found` if the path is absent or tombstoned. `400 is_directory` if it
is a directory.

### `PUT /v1/files/{path}`

Body is the raw file content. No multipart, no encoding.

| Request header | Meaning |
|---|---|
| `X-Base-Rev` | The revision you believe is current for this path. Omit or send `0` to mean "I believe this path does not exist" |
| `X-Content-SHA256` | Optional. The hash of exactly the bytes being sent. If it does not match what arrives, the upload is refused with `422 hash_mismatch` and nothing is stored |

**Send the hash, and hash a snapshot rather than the live file.** This is not
about the network, which is already checksummed. It is about the file changing
while the client reads it. An editor saving over a file produces bytes that
belong to no version of it, and a client that uploads straight from the live
file can send them: `URLSession.upload(fromFile:)` on macOS fixes the content
length when the request is made, and a file truncated from 4 MB to 5 bytes
mid-upload is still sent as 4 MB, the remainder arriving as NULs. Copy the file
first, hash the copy, upload the copy, and declare that hash.

- `201 Created` if the path was absent or tombstoned.
- `200 OK` if it existed and your `X-Base-Rev` matched.
- `409 conflict` if it did not match. **The body is still stored**, under a
  different name. See §7.

Success body:

```json
{
  "path": "notes.md", "rev": 43, "size": 11,
  "sha256": "b94d27b9…", "mtime": 1785194027083, "type": "file"
}
```

Writes are atomic: content lands in a temporary file, is fsynced, then renamed
into place. A reader sees the whole old file or the whole new one, never a
partial write, and an interrupted upload leaves nothing behind.

### `DELETE /v1/files/{path}` · `DELETE /v1/dirs/{path}`

Requires a matching `X-Base-Rev`. Deleting a path whose latest version you have
not seen would discard another machine's edit, so a mismatch is `409 stale`:
fetch the current version, decide, and try again.

Deleting a directory that still has indexed children is `409 not_empty` —
delete the children first.

For files, the previous content goes to the trash (§8) before the tombstone is
recorded.

### `PUT /v1/dirs/{path}`

Creates a directory, including missing parents. `201` when created, `200` when
it already existed (creating twice is not an error). No body.

Directories exist in the protocol so that **empty** directories survive a sync;
a directory containing files is created implicitly by the files inside it.

A directory's mtime is deliberately **not** tracked. It changes every time a
child is added or removed, so tracking it would burn a revision, and wake every
connected machine, for every single file created anywhere in the tree.

### `GET /v1/events`

Server-Sent Events. Emits the current revision on connect, then again whenever
the tree changes:

```
event: rev
data: {"rev":43}

: heartbeat

event: rev
data: {"rev":44}
```

Comment lines (`: heartbeat`, every 25 seconds) keep proxies and NAT tables from
dropping an idle connection.

**The stream is a hint to look, never a channel that has to be reliable.** The
payload is only a number; the client always follows up with `GET /v1/changes`.
Events may therefore be coalesced, delayed or dropped without any risk of a
missed update. Treat a disconnect as normal: reconnect with exponential backoff,
and let the next `since=<rev>` recover whatever happened meanwhile.

Events fire for changes made directly on the server's filesystem too, not only
for those arriving through this API.

### `GET /v1/ignore` · `PUT /v1/ignore`

The ignore rules live on the server so every machine filters identically: a
rule added on one Mac takes effect everywhere without touching the others.

```json
{ "rules": "# comment\n.DS_Store\n*.tmp\n", "version": 1785194027083 }
```

`version` is the unix-millisecond timestamp of the last save, or `0` when the
server is still serving its defaults. It also appears as the `ETag`, so a client
can cheaply detect a change.

`PUT` takes `{"rules": "..."}`. Syntax is gitignore-style globs, one per line,
matched against the path relative to the root; blank lines and `#` comments are
skipped.

Clients should apply these rules **in addition to** their own platform noise
list, never instead of it. They should re-read them at the start of every cycle:
a rule saved on another machine has to reach this one before it decides what to
upload, and before it reads the server acting on that rule as a mass deletion.

**A save is retroactive.** The rules are not a filter clients apply on their own
— the server enforces them too. Saving them:

- takes every path they now exclude out of the index, as a tombstone, so every
  client stops listing it;
- moves that content to the trash, where it stays recoverable;
- keeps it out from then on. A file matching a rule is not indexed again,
  whether it arrives through this API or is written straight to the volume.

The response reports how many paths went:

```json
{ "rules": "...", "version": 1785194027083, "purged": 167 }
```

`purged` is omitted when nothing matched. A client sees the removals as ordinary
tombstones, which its own copy of the rules filters out of the change set — so
what a rule takes off the server stays on the machines that hold it.

A rule that names a scope explicitly (`mac/build/`) is read differently by the
two sides: the server sees `mac/build`, and the device syncing `mac` sees
`build`. The server only drops a path when every reading of it — its full name,
and its name as seen from each device scope containing it — excludes it.
Otherwise the server would tombstone a directory a device is still uploading,
and the two would undo each other indefinitely.

### `GET /v1/trash` · `POST /v1/trash/restore`

```json
{ "items": [
  { "id": "20260727T201347.051_notes.md",
    "path": "notes.md",
    "deleted_at": "2026-07-27T20:13:47.051Z",
    "size": 11 }
] }
```

`deleted_at` is RFC 3339, unlike `mtime` elsewhere, because it is a timestamp
for humans rather than a value to compare against a filesystem.

`POST /v1/trash/restore` with `{"id": "..."}` puts the content back at its
original path and assigns it a fresh revision. If something already occupies
that path the request fails with `409 occupied` and nothing moves: restoring
must never be the operation that loses data.

---

## 7. Conflicts

Two machines edit `notes.md` from revision 40. The first `PUT` carries
`X-Base-Rev: 40`, matches, and becomes revision 41. The second also carries
`X-Base-Rev: 40`, which no longer matches.

The server does not pick a winner and does not reject the upload. It stores the
incoming body under a generated name and answers `409`:

```json
{
  "error": "conflict",
  "message": "path changed since rev 40 (now 41); body stored as \"notes.conflict-iMac-20260727-201347.md\"",
  "conflict": "notes.conflict-iMac-20260727-201347.md",
  "path": "notes.conflict-iMac-20260727-201347.md",
  "rev": 42, "size": 84, "sha256": "…", "mtime": 1785194027083, "type": "file"
}
```

The name is `<stem>.conflict-<device>-<YYYYMMDD-HHMMSS><ext>`. Keeping the
original extension means the copy still opens in the right application; the
device name and timestamp make its origin obvious without opening it.

Both files then exist and both sync everywhere. No content is ever lost, and no
automatic merge is attempted: resolving the difference is a human decision, and
the protocol's job is to make sure the human still has both versions to look at.

A client should surface conflicts prominently rather than treat `409` as a
routine error.

---

## 8. Trash

Before any overwrite and any delete, the previous content is moved (not copied,
so it is instant and cannot half-succeed) into a retention area. It is purged
after `TRASH_DAYS` days, 30 by default.

The trash is not part of the synced tree and never appears in `/v1/changes`.

Tombstones are pruned separately after `TOMBSTONE_DAYS` days, 90 by default. A
client offline for longer than that cannot be brought up to date incrementally
and must resync from `since=0`.

---

## 9. Writing a client

The recommended loop, and the failure modes worth knowing about.

### Local state

Persist, per path, the last state you successfully synced: `rev`, `size`,
`mtime`, `sha256`. Plus one global `last_rev`. That is enough to tell "changed
locally" from "changed remotely" without asking the server anything.

### Pulling

1. `GET /v1/changes?since=last_rev`.
2. Apply entries **in order**. For a tombstone, delete locally. For a file,
   download to a temporary file, fsync, then rename into place; never write
   directly to the destination.
3. Page while `more` is true, then store `current_rev` as `last_rev`.

### Pushing

For each locally changed path (detect via size and mtime against your stored
state, and only then compute a hash):

1. `PUT /v1/files/<path>` with `X-Base-Rev` set to the `rev` in your stored
   state, or `0` if you have never seen this path.
2. On `409 conflict`, record the conflict for the user; the next pull brings
   down both versions.

### Three things that will bite

**Echo suppression.** A file you just downloaded looks, to your own filesystem
watcher, exactly like a file the user edited. Without a set of "paths I just
wrote" to discard those events, the client will push back what it just pulled,
forever.

**Do not trust mtime alone.** Use it as a cheap filter to decide what is worth
hashing, then compare hashes. Content that is identical despite a touched mtime
must not produce an upload, or a stray `touch` wakes every machine for nothing.

**Delete guards.** A bug in your state handling, or a server whose volume failed
to mount, can produce a change set that deletes everything. Refuse to apply a
pull that would delete more than some threshold of the local tree, and ask the
user instead. This has cost nothing to implement and saves everything exactly
once.

### Ordering

Apply pulls before pushes within a cycle. It keeps `X-Base-Rev` fresh and turns
what would have been a conflict into an ordinary update whenever the two edits
did not actually overlap in time.
