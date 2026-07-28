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

CI publishes a multi-architecture image (amd64 and arm64) to GitHub Packages on
every push to `main`, so a server does not have to build anything:

```bash
docker pull ghcr.io/schmidt-gabriel/homesync:latest
```

To run that instead of building locally, replace `build: ./server` in
`compose.yaml` with `image: ghcr.io/schmidt-gabriel/homesync:latest`. Tagged
releases also publish `v1.2.3` and `v1.2` tags; pin to one of those rather than
`latest` if you would rather choose when to upgrade.

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
ADMIN_PASSWORD=something docker compose up -d
```

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

One static binary and a systemd user unit. See
[`clients/linux/README.md`](clients/linux/README.md).

```bash
cd clients/linux
go build -o homesync-client ./cmd/homesync-client

export HOMESYNC_URL=http://homelab.local:8420
export HOMESYNC_TOKEN=...    # from `homesync device add <name>`
./homesync-client
```

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
