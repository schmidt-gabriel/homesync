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
server/       Go server and its Docker image
clients/mac/  macOS menu-bar client (Swift)
docs/         PROTOCOL.md — the contract, and the source of truth
conformance/  executable protocol test suite
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
behaviour.

## Development

```bash
cd server && go test ./... && go vet ./...
cd clients/mac/HomeSyncKit && swift test
```
