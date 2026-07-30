# HomeSync

A file sync server and its clients. The server holds the intelligence, because
more than one machine reaches the same files; clients keep one number and ask
"what changed since N?".

## Layout

```
server/        Go: HTTP API, SQLite index, fsnotify, trash, embedded admin UI
clients/mac/   Swift: HomeSyncKit (the engine, no UI) + HomeSync.xcodeproj
clients/linux/ Go: homesync-client daemon + systemd user unit
conformance/   Go: the executable contract, run against any client
docs/PROTOCOL.md   The contract itself. Written before the second client.
.github/workflows/ server.yml, mac.yml, linux.yml, release.yml
```

`clients/mac/`, not `mac/`: another platform goes in beside it without moving
anything.

## Working agreements

**Branch and PR for everything.** No commits straight to `main` — the tags need
to say what went into them. One branch per change, `gh pr create`, and the PR
body explains what was wrong and how it was verified.

**Run it, do not reason about it.** Every claim about behaviour comes from
having executed it. The local server is the place to do that:

```bash
docker compose up -d --build
docker compose exec -T homesync homesync device add mac mac   # prints a token once
```

**A new test must be shown to fail.** Write the test, revert the fix, watch it
fail with the message the user reported, restore the fix. A test that has never
failed has not been tested. This has caught at least three assertions of mine
that could not fail.

**Comments say why, never what.** The code already says what. If a line looks
arbitrary, the comment explains the failure that put it there — the URLSession
padding bug, the firmlink at `/var`, `hashValue` being seeded per process. Match
the density of the file you are in.

## Commands

```bash
# Server
cd server && go vet ./... && go test -race ./...

# Conformance, against a running local server
cd conformance && HOMESYNC_URL=http://localhost:8420 HOMESYNC_TOKEN=$TOKEN go test ./...

# The Swift engine, against the same server
cd clients/mac/HomeSyncKit && \
  HOMESYNC_TEST_URL=http://localhost:8420 HOMESYNC_TEST_TOKEN=$TOKEN swift test

# The Linux client: unit tests, then the real binary driven end to end
cd clients/linux && go test -race ./...
cd conformance && go build -o /tmp/homesync-client ../clients/linux/cmd/homesync-client && \
  HOMESYNC_URL=http://localhost:8420 HOMESYNC_TOKEN=$TOKEN HOMESYNC_SCOPE=linux \
  HOMESYNC_CLIENT_CMD=/tmp/homesync-client go test -run TestClientConformance ./...

# The app
cd clients/mac && xcodebuild -scheme HomeSync -configuration Release CODE_SIGNING_ALLOWED=NO
```

The Swift tests skip themselves without `HOMESYNC_TEST_URL`, so a bare
`swift test` proves nothing. Point it at a server.

## Things that bite

**Scopes.** Each device syncs one subtree and believes it is at the root. Paths
are resolved in on the way through and stripped out on the way back. A scope
filter belongs in the SQL, not applied to the result — dropping rows afterwards
gives an empty page with `more: true`, and the paging rule has no last entry to
resume from.

**Case collisions are checked before writing.** On a case-insensitive volume,
`PUT NOTES.md` destroys `notes.md` before any 409 can be returned.

**Unicode.** The server's index is NFC; macOS hands out NFD. Normalise at every
boundary. Swift's `String` compares by canonical equivalence, so an NFC/NFD test
that compares two `String`s is comparing a value to itself — assert on UTF-8
bytes.

**Never trust a file while uploading it.** `URLSession.upload(fromFile:)` fixes
the length when the request is made and pads the body if the file shrinks
underneath. Snapshot first, then hash and send the snapshot. The server verifies
`X-Content-SHA256` and refuses a mismatch.

**Never animate the menu bar icon by setting its image.** Driving a rotation
from a timer put ~98% of the main thread inside `NSStatusBarButton.setImage:`
and froze the app under load. Use `.symbolEffect`.

**The client conformance suite needs a scope to itself, fresh each run.** It
writes fixed filenames and asserts on what comes back, so a scope still holding
the last run's `from-server.txt` fails for reasons that have nothing to do with
the client. It also starts the client in its own process group and signals the
group: killing only `cmd.Process` reaches the shell, and where `/bin/sh` forks
rather than execs (dash, so every Linux runner) the client outlives the test.

**Ignore rules are enforced by the server, not only by the clients.** They live
in the meta table, and the scanner, the watcher and the API all read the same
parsed copy. Saving them is retroactive: what they now exclude is tombstoned and
moved to the trash. Two things follow. A path is only dropped when *every*
reading of it excludes it — its full name and its name inside each device scope
— or the server would tombstone what a scoped device is still uploading, and the
two would undo each other forever. And clients must re-read the document every
cycle: one that keeps its launch-time copy reads the purge as a mass deletion
and trips the delete guard.

**Encryption at rest is server-side.** The server holds the key, so it defends a
stolen disk or a copied backup, not a compromised server. Sizes and hashes in
the index always describe plaintext; the bytes on disk are longer.
