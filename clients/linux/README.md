# HomeSync — Linux client

A daemon that keeps a folder in step with a HomeSync server. One static binary,
no runtime to install, no cgo.

It was written against [`docs/PROTOCOL.md`](../../docs/PROTOCOL.md) rather than
against the Mac client, which is the reason the protocol was written down
first. The two agree because they both implement the document, and both prove
it by passing the same conformance suite.

## Build

```bash
cd clients/linux
go build -o homesync-client ./cmd/homesync-client
```

Cross-compiling from anywhere, since there is no cgo:

```bash
GOOS=linux GOARCH=amd64 go build -o homesync-client ./cmd/homesync-client
GOOS=linux GOARCH=arm64 go build -o homesync-client ./cmd/homesync-client
```

## Run

Register the machine on the server and keep the token it prints — it is shown
once, and only its hash is stored.

```bash
docker compose exec homesync homesync device add thinkpad
```

Then:

```bash
export HOMESYNC_URL=http://homelab.local:8420
export HOMESYNC_TOKEN=...
export HOMESYNC_ROOT=~/HomeSync

./homesync-client
```

| Setting | Flag | Default |
|---|---|---|
| Server URL | `-url` | `HOMESYNC_URL` |
| Device token | `-token` | `HOMESYNC_TOKEN` |
| Folder | `-root` | `HOMESYNC_ROOT`, else `~/HomeSync` |
| Name in conflict copies | `-device` | `HOMESYNC_DEVICE`, else the hostname |
| One cycle then exit | `-once` | off |
| Poll interval | — | `HOMESYNC_POLL_INTERVAL`, default `5m` |
| Mass-delete limit | — | `HOMESYNC_MAX_DELETES`, default `100` |
| Log level | — | `LOG_LEVEL`, default `info` |

Prefer the environment to `-token`: a flag is visible to anyone who can list
processes.

## As a service

`homesync.service` is a systemd **user** unit — it syncs one person's folder
with their permissions and has no reason to run as root. The file itself
explains the two-minute setup.

## How it behaves

**Both ways, continuously.** inotify gives immediacy; a poll every five minutes
is what actually guarantees correctness, because inotify drops events under
load and knows nothing about what happened while the process was down. A
server-sent event stream carries the other direction, and losing it never loses
data: the next cycle asks for everything since the revision it holds.

**Conflicts keep both versions.** When two machines edit the same file from the
same base, nothing is discarded and nothing is merged. The loser is renamed to
`notes.conflict-thinkpad-20260727-201347.md` and both files sync everywhere.
Resolving the difference is a human decision.

**It refuses an implausible mass deletion.** If a cycle would remove more than
`HOMESYNC_MAX_DELETES` local files — or more than a quarter of what it tracks —
it stops and says so rather than carrying it out. A damaged state database and a
server whose volume failed to mount both look exactly like "the user deleted
everything".

**It will not delete a file it has no record of.** Such a file is either
something you put there or something left behind when the state was lost, and a
lost state is precisely when the server's tombstones stop describing this
machine. It is kept and uploaded instead.

**Ignore rules come from the server**, so a pattern added on one machine applies
everywhere, and are applied *in addition to* a built-in list of editor and
desktop noise — including the macOS entries, because a Linux box and a Mac share
a tree and a `.DS_Store` only one of them filters travels anyway.

**Symlinks are skipped**, matching the server. Handling them properly means
deciding what to do about targets outside the root, and that is not worth the
complexity yet.

## Verifying it

Against a server in Docker:

```bash
docker compose up -d --build
TOKEN=$(docker compose exec -T homesync homesync device add linux linux | grep -oE '[A-Za-z0-9_-]{43}')

cd clients/linux && go test ./...

cd ../../conformance
go build -o /tmp/homesync-client ../clients/linux/cmd/homesync-client
HOMESYNC_URL=http://localhost:8420 HOMESYNC_TOKEN=$TOKEN HOMESYNC_SCOPE=linux \
  HOMESYNC_CLIENT_CMD=/tmp/homesync-client go test -run TestClientConformance ./...
```

The conformance run is the one that matters. It drives the real binary against
a real server and checks both directions, deletions, nested directories, ignore
rules and conflicts.

Give it a scope of its own, and a fresh one each time. It writes fixed
filenames and asserts on what comes back, so a scope still holding the previous
run's `from-server.txt` fails in ways that have nothing to do with the client.
CI starts a new server per run; locally, `docker compose down -v` between runs
does the same job.
