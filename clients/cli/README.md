# HomeSync — CLI client

A daemon that keeps a folder in step with a HomeSync server. One static binary,
no runtime to install, no cgo.

It was written against [`docs/PROTOCOL.md`](../../docs/PROTOCOL.md) rather than
against the Mac client, which is the reason the protocol was written down
first. The two agree because they both implement the document, and both prove
it by passing the same conformance suite.

- [Install](#install)
- [First run](#first-run)
- [Run it as a service](#run-it-as-a-service)
- [Where everything lives](#where-everything-lives)
- [Everyday operations](#everyday-operations)
- [Settings](#settings)
- [How it behaves](#how-it-behaves)
- [When something looks wrong](#when-something-looks-wrong)
- [Verifying it](#verifying-it)

## Install

```bash
cd clients/cli
go build -o homesync-client ./cmd/homesync-client
sudo install -m 755 homesync-client /usr/local/bin/
```

There is no cgo, so it cross-compiles from any machine to any of them, which is
the usual way to get a binary onto a Raspberry Pi:

```bash
GOOS=linux  GOARCH=amd64 go build -o homesync-client ./cmd/homesync-client
GOOS=linux  GOARCH=arm64 go build -o homesync-client ./cmd/homesync-client
GOOS=darwin GOARCH=arm64 go build -o homesync-client ./cmd/homesync-client
```

CI builds every one of those on each change, so the list is checked rather than
claimed.

## First run

**1. Register this machine on the server.** The token is printed once and only
its hash is stored, so it cannot be recovered later; if you lose it, issue a new
one from the Devices tab of the admin page.

```bash
docker compose exec homesync homesync device add thinkpad
```

That gives the machine a folder of its own on the server, named after it. Two
devices given the *same* folder sync the same files:

```bash
docker compose exec homesync homesync device add thinkpad shared
```

**2. Run it in the foreground once**, before making it a service, so a wrong
address or a mistyped token is a message on your terminal rather than something
to go hunting for in the journal:

```bash
export HOMESYNC_URL=http://homelab.local:8420
export HOMESYNC_TOKEN=...

homesync-client -once
```

`-once` runs a single cycle and exits, saying what it did:

```
level=INFO msg=syncing root=/home/you/HomeSync server=http://homelab.local:8420
level=INFO msg="cycle complete" did="up 1"
```

Drop the `-once` and it stays up, syncing continuously.

## Run it as a service

On Linux, [`linux/homesync.service`](linux/homesync.service) is a systemd
**user** unit. It syncs one person's folder with their permissions and has no
business running as root.

```bash
mkdir -p ~/.config/homesync ~/.config/systemd/user

cat > ~/.config/homesync/env <<'EOF'
HOMESYNC_URL=http://homelab.local:8420
HOMESYNC_TOKEN=paste-the-token-here
HOMESYNC_ROOT=/home/you/HomeSync
EOF
chmod 600 ~/.config/homesync/env

cp linux/homesync.service ~/.config/systemd/user/
systemctl --user enable --now homesync.service
journalctl --user -u homesync -f
```

The token lives in that env file, mode 600, rather than in the unit (which ends
up world-readable) or on the command line (where anyone who can run `ps` reads
it).

Add `loginctl enable-linger $USER` if the folder should keep syncing while
nobody is logged in. Without it, systemd stops your user services at logout.

No unit is shipped for anything else, because a plausible-looking launchd plist
that nobody has run is worse than none. Elsewhere, hand it to whatever already
supervises your user processes with the same three variables in its
environment. Whatever that is, give it a restart policy: the client exits on a
revoked token rather than running blind, and a supervisor that treats the exit
as final leaves the folder quietly unsynced.

## Where everything lives

| What | Where | Change it with |
|---|---|---|
| The synced folder | `~/HomeSync` | `HOMESYNC_ROOT` or `-root` |
| What it remembers about that folder | `<config>/homesync/state-<hash>.sqlite` | not meant to be edited |
| Server address, token, folder | `~/.config/homesync/env`, read by the unit | edit, then restart the service |
| Logs | the journal, or wherever stderr goes | `journalctl --user -u homesync` |

`<config>` is the system's own config directory — `~/.config` on Linux and the
BSDs, `~/Library/Application Support` on macOS — so a machine that runs both
this and some other tool keeps them where each platform expects.

`<hash>` is the first 16 hex characters of the SHA-256 of the folder's resolved
path, so each folder keeps its own record and switching between two never mixes
them up.

That state database holds what this machine has already agreed with the server:
one revision number, and the size, modification time and hash of every path.
Deleting it loses no files. It makes the client treat everything as new, which
means a full comparison against the server and an upload of anything it cannot
match there. Safe, but not free on a large folder.

### Changing the synced folder

Point it somewhere else and restart:

```bash
sed -i 's|^HOMESYNC_ROOT=.*|HOMESYNC_ROOT=/home/you/Documents/HomeSync|' ~/.config/homesync/env
systemctl --user restart homesync
```

The new folder gets its own state, so the client downloads the whole tree into
it, and anything already there that the server does not have goes up. The old
folder is left exactly as it was: nothing is moved and nothing is deleted. If
you meant to move rather than copy, delete it yourself afterwards, along with
its `state-<hash>.sqlite`.

### Changing the server or the token

Same file, same restart. A revoked token shows up as `401` in the journal, and
nothing syncs until a valid one is in place.

## Everyday operations

**Sync now.** The daemon already syncs on a local change, on a server event and
every five minutes, but sometimes you want it to look right now:

```bash
pkill -USR1 homesync-client            # anywhere
systemctl --user kill -s USR1 homesync # under systemd
```

It logs
`sync requested` and runs one cycle. Restarting the service also works, but it
throws away a healthy event stream and a warm state to ask a question the
running process could answer.

**See what it is doing.**

```bash
journalctl --user -u homesync -f
systemctl --user status homesync
```

A cycle that found nothing to do logs nothing, on purpose. `LOG_LEVEL=debug`
adds little here — one line when the server's event stream drops — because the
client is quiet by design; it is the *server* that logs a line per request at
debug level, and that is where to look when the question is what was asked of
it.

**Change what is never synced.** The ignore rules live on the server, so a
pattern added anywhere applies to every machine. Edit them in the admin page,
under *Ignore rules*. Saving is retroactive: whatever the rules now exclude is
taken out of the server's index and moved to its trash, while the copies on this
machine stay where they are. The client re-reads the document at the start of
every cycle, so a rule saved elsewhere takes effect within one cycle, without a
restart.

**Check which server you are talking to.**

```bash
curl -s http://homelab.local:8420/healthz
{"rev":109887,"status":"ok","version":"1.1.2"}
```

No token needed. `version` is the release that server is running.

**Uninstall.**

```bash
systemctl --user disable --now homesync
rm ~/.config/systemd/user/homesync.service ~/.config/homesync/env
rm ~/.config/homesync/state-*.sqlite*
sudo rm /usr/local/bin/homesync-client
```

The synced folder is left alone. Removing the state files discards what the
client remembered, never the files themselves.

## Settings

| Setting | Flag | Environment | Default |
|---|---|---|---|
| Server URL | `-url` | `HOMESYNC_URL` | — |
| Device token | `-token` | `HOMESYNC_TOKEN` | — |
| Folder | `-root` | `HOMESYNC_ROOT` | `~/HomeSync` |
| Name in conflict copies | `-device` | `HOMESYNC_DEVICE` | the hostname |
| One cycle then exit | `-once` | — | off |
| Poll interval | — | `HOMESYNC_POLL_INTERVAL` | `5m` |
| Mass-delete limit | — | `HOMESYNC_MAX_DELETES` | `100` |
| Log level | — | `LOG_LEVEL` | `info` |

Prefer the environment to `-token`: a flag is visible to anyone who can list
processes.

Signals: `SIGUSR1` syncs now; `SIGINT` and `SIGTERM` stop it.

## How it behaves

**Both ways, continuously.** The file-system watcher — inotify on Linux, kqueue
on macOS and the BSDs — gives immediacy; a poll every five minutes is what
actually guarantees correctness, because those interfaces drop events under
load and know nothing about what happened while the process was down. A
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

## When something looks wrong

**`refusing to delete N local files in one go`.** The guard above tripped, and
it is not retried on a timer: a guard that trips every five minutes is noise
rather than a warning. Find out why the server thinks that much is gone. Its
volume not mounted, a device's folder changed, or a broad ignore rule saved on
another machine all produce exactly this. When the answer really is "yes, delete
them", raise `HOMESYNC_MAX_DELETES` for one run.

**Files come back after being deleted.** Delete them while the client is
running, not while it is stopped. A file removed from a stopped client's folder
is, to the next cycle, a file the server has and this machine does not.

**`cannot reach the server: unauthorized (401)`.** The token was revoked, or
belongs to a device that no longer exists. The client exits on this rather than
running blind, and systemd restarts it every ten seconds, so the journal fills
with the same line until a valid token is in the env file. Issue one from the
Devices tab.

**Nothing syncs, and there is no error.** Check the folder it actually opened.
It says so on startup, and a typo in `HOMESYNC_ROOT` gives you a new empty
folder and a perfectly healthy client syncing it:

```bash
journalctl --user -u homesync | grep msg=syncing
level=INFO msg=syncing root=/home/you/HomeSync server=http://homelab.local:8420
```

**A file never reaches the server.** It is probably excluded. The ignore rules
are shared, so check them in the admin page, and remember that the built-in
noise list covers editor scratch files (`*.swp`, `*~`, `.#*`) whatever the
server's document says.

## Verifying it

Against a server in Docker:

```bash
docker compose up -d --build
TOKEN=$(docker compose exec -T homesync homesync device add cli cli | grep -oE '[A-Za-z0-9_-]{43}')

cd clients/cli
go test ./...
go build -o /tmp/homesync-client ./cmd/homesync-client

cd ../../conformance
HOMESYNC_URL=http://localhost:8420 HOMESYNC_TOKEN=$TOKEN HOMESYNC_SCOPE=cli \
  HOMESYNC_CLIENT_CMD=/tmp/homesync-client go test -run TestClientConformance ./...
```

Build the binary from `clients/cli`, not by relative path from `conformance`:
they are separate modules, and `go build ../clients/cli/...` refuses with
*outside main module*.

That works on a Mac as well as on Linux, against the same server in Docker, and
is how the claim in the first paragraph was checked rather than assumed.

The conformance run is the one that matters. It drives the real binary against
a real server and checks both directions, deletions, nested directories, ignore
rules and conflicts.

Give it a scope of its own, and a fresh one each time. It writes fixed
filenames and asserts on what comes back, so a scope still holding the previous
run's `from-server.txt` fails in ways that have nothing to do with the client.
CI starts a new server per run; locally, `docker compose down -v` between runs
does the same job.
