<p align="center">
  <img width="256" height="256" alt="appstore" src="https://github.com/user-attachments/assets/c0950cde-f461-4620-884f-dfbbef6d7f3b" />
</p>

# HomeSync

File sync between your own machines, through a server you run.

The server owns the files and a global revision counter. A client remembers a
single number, the last revision it saw, and asks what changed after it. That
is the whole design, and it is why adding a third or fifth machine costs
nothing: no client tracks any other client.

- **Bidirectional**, including deletions.
- **Never loses content.** Concurrent edits produce a `.conflict-*` copy rather
  than a winner. Overwrites and deletes go to a trash with a retention window.
- **Real-time.** Changes propagate between machines in seconds over SSE.
- **Small enough to debug.** One Go binary and one Swift package.

## Layout

```
server/        Go server and its Docker image
clients/mac/   macOS menu-bar client (Swift)
clients/linux/ Linux daemon (Go)
docs/          PROTOCOL.md — the contract, and the source of truth
conformance/   executable protocol test suite
```

## Running it

```bash
docker compose up -d --build
```

That is the whole setup: the bind mounts are relative, so nothing has to be
prepared on the host first. Files live in `./data`, the index in `./config`.

### Using the published image

CI publishes a multi-architecture image (amd64 and arm64) to GitHub Packages
for every tagged release, so a server does not have to build anything:

```bash
docker pull ghcr.io/schmidt-gabriel/homesync:latest
```

To run that instead of building locally, replace `build: ./server` in
`compose.yaml` with `image: ghcr.io/schmidt-gabriel/homesync:latest`. Each
release also publishes `1.2.3` and `1.2` tags; pin to one of those rather than
`latest` if you would rather choose when to upgrade. A server reports which one
it is running on `/healthz` and at the top of the admin page.

Register a machine and get its token:

```bash
docker compose exec homesync homesync device add "MacBook Pro"
```

The token is printed once. Only its SHA-256 is stored, so it cannot be
recovered later; if you lose it, revoke the device and add it again.

### Admin UI

Set `ADMIN_PASSWORD` and the server serves a management page at
`http://localhost:8420/`: devices, a file browser, the trash, and the shared
ignore rules. With no password set, none of those routes are registered at all.

```bash
ADMIN_USER=admin ADMIN_PASSWORD=something docker compose up -d
```

`ADMIN_USER` defaults to `admin`. The username and the password are both
checked, both in constant time, and a wrong one of either gives the same
answer — knowing which half you got right is a head start nobody needs.

`ADMIN_NO_AUTH=true` serves the page with no login at all. It is a real option
and it is genuinely convenient on a network you control, but be clear about
what it opens: anyone who can reach the port can issue device tokens, read
every filename, and empty the trash. The server says so on startup and the page
carries a banner while it is on.

### Encrypting the files at rest

Off by default. With `ENCRYPTION_KEY` set, file contents are encrypted before
they touch the volume:

```bash
docker compose run --rm homesync key generate   # prints a key, store it
ENCRYPTION_KEY=... docker compose up -d
```

**What this protects.** A stolen disk, a copied backup, another container that
has the volume mounted. The server holds the key, so it does not protect
against someone who has the running server, and it is not end-to-end: clients
send and receive plaintext, which is what HTTPS is for.

**What stays readable.** Filenames and the shape of the tree. Encrypting those
would mean the volume no longer shows what is in it, and the index describes
them in the clear anyway.

**Losing the key loses the files.** Nothing on the server can recover them.

Turning it on does not touch what is already on the volume — the server reads
either form, so nothing breaks the moment you set the key. To convert:

```bash
docker compose stop
docker compose run --rm homesync key encrypt   # or: key decrypt
docker compose start
```

Both are safe to interrupt and safe to run twice. Modification times are
carried across, so a converted tree does not look modified and no client
re-downloads anything.

### Configuration

Copy `.env.sample` to `.env`. Every value has a working default; see the sample
for what each one does.

## Installing the Mac client

```bash
brew install --cask schmidt-gabriel/tap/homesync
xattr -dr com.apple.quarantine /Applications/HomeSync.app
```

The second line is not optional. The build is not signed with an Apple
Developer ID, so macOS flags it as quarantined. Homebrew's `--no-quarantine`
does not help: it stopped being a command-line flag, and setting it through
`HOMEBREW_CASK_OPTS` did not prevent the flag either when tested.

Then:

1. **Register the Mac on the server** and copy the token it prints. It is shown
   once and only its hash is stored, so it cannot be recovered later.

   ```bash
   docker compose exec homesync homesync device add "MacBook"
   ```

   The admin page does the same thing with a form, if you would rather.

2. **Open HomeSync** from Applications. It has no Dock icon: look for the
   circle in the menu bar.

3. **Menu bar icon → Settings → Server.** Paste the address and the token, then
   press Apply. It tells you whether it actually connected rather than just
   saving.

4. **General tab**, if you want the folder somewhere other than
   `~/Library/CloudStorage/HomeSync`, and to start HomeSync at login.

Files then sync both ways on their own: local edits go up within a second or so,
and anything another machine changes arrives about as fast.

### Adding a second Mac

Repeat the above with a different device name. By default each device gets its
own folder on the server and the two do not see each other. To make them sync
the *same* files, give both the same folder: either pass it when registering,

```bash
docker compose exec homesync homesync device add "iMac" MacBook
```

or change it later from the Devices tab of the admin page. A device whose
folder changes downloads everything again from scratch.

### Building it yourself instead

```bash
cd clients/mac
xcodebuild -project HomeSync.xcodeproj -scheme HomeSync -configuration Release \
  -derivedDataPath build build
cp -R build/Build/Products/Release/HomeSync.app /Applications/
```

## Installing the Linux client

One static binary and a systemd user unit.

1. **Register the machine** and copy the token, same as for a Mac:

   ```bash
   docker compose exec homesync homesync device add thinkpad
   ```

2. **Build and install.** There is no cgo, so it cross-compiles from anywhere:
   `GOOS=linux GOARCH=arm64` produces a binary for a Raspberry Pi.

   ```bash
   cd clients/linux
   go build -o homesync-client ./cmd/homesync-client
   sudo install -m 755 homesync-client /usr/local/bin/
   ```

3. **Try it once in the foreground**, so a wrong address says so on your
   terminal:

   ```bash
   export HOMESYNC_URL=http://homelab.local:8420
   export HOMESYNC_TOKEN=...
   homesync-client -once
   ```

4. **Then make it a service.** The token goes in an env file rather than in the
   unit or on the command line:

   ```bash
   mkdir -p ~/.config/homesync ~/.config/systemd/user
   printf 'HOMESYNC_URL=%s\nHOMESYNC_TOKEN=%s\nHOMESYNC_ROOT=%s\n' \
     "$HOMESYNC_URL" "$HOMESYNC_TOKEN" "$HOME/HomeSync" > ~/.config/homesync/env
   chmod 600 ~/.config/homesync/env

   cp homesync.service ~/.config/systemd/user/
   systemctl --user enable --now homesync.service
   journalctl --user -u homesync -f
   ```

The folder defaults to `~/HomeSync`; set `HOMESYNC_ROOT` to put it elsewhere.
`systemctl --user kill -s USR1 homesync` syncs now, without waiting for the
five-minute poll. [`clients/linux/README.md`](clients/linux/README.md) covers
the rest: where the state lives, moving the folder, reading the logs, and what
to do when something looks wrong.

It was written against the protocol document rather than against the Mac
client, and passes the same conformance suite. That is the whole argument for
having written the protocol down before there was a second client.

## A word on transport security

The device token is a bearer credential: whoever holds it can read and write
everything. Over plain HTTP it is visible to anything on the network path.

That is acceptable on a trusted local network and nowhere else. For anything
reachable from outside, set `TLS_CERT` and `TLS_KEY`, or put the server behind
a reverse proxy that terminates TLS. The admin UI shows a banner whenever it is
being served without it.

## Writing another client

Read [`docs/PROTOCOL.md`](docs/PROTOCOL.md). It documents the endpoints, the
conflict rule, the path rules (Unicode normalisation, case collisions, names
Windows cannot represent) and the recommended client loop, including the three
mistakes that are easy to make.

Prove the result against `conformance/` rather than against another client's
behaviour. `clients/linux/` is the worked example: point
`HOMESYNC_CLIENT_CMD` at any binary that takes `HOMESYNC_URL`,
`HOMESYNC_TOKEN` and `HOMESYNC_ROOT` from its environment and keeps syncing
until it is killed.

## Development

```bash
cd server && go test ./... && go vet ./...
cd clients/linux && go test ./... && go vet ./...
cd clients/mac/HomeSyncKit && swift test
```
