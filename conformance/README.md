# Conformance suite

[`docs/PROTOCOL.md`](../docs/PROTOCOL.md) written as executable statements.

This package imports nothing from the Go server and speaks only HTTP, so it
validates *a* HomeSync implementation rather than *the* one in this repository.
A server or client written in another language proves itself by passing these
tests, not by being compared against an existing implementation.

## Validating a server

```bash
go test ./...
```

With no configuration it builds `../server` and runs it against a temporary
directory, so this works from a fresh clone.

To test a server that is already running, for example the built container:

```bash
docker compose up -d --build
TOKEN=$(docker compose exec -T homesync homesync device add ci | grep -oE '[A-Za-z0-9_-]{43}')

HOMESYNC_URL=http://localhost:8420 \
HOMESYNC_TOKEN=$TOKEN \
HOMESYNC_DATA_DIR=$PWD/data \
  go test ./conformance/...
```

`HOMESYNC_DATA_DIR` is optional. Without it the tests that write straight into
the server's volume, checking that changes made outside the API are picked up,
are skipped rather than silently passing.

## Validating a client

Skipped unless `HOMESYNC_CLIENT_CMD` is set. The command is launched once and
expected to keep syncing until killed, configured entirely through three
environment variables:

| Variable | Meaning |
|---|---|
| `HOMESYNC_URL` | Server to talk to |
| `HOMESYNC_TOKEN` | Device token |
| `HOMESYNC_ROOT` | Local directory to keep in sync |

```bash
HOMESYNC_CLIENT_CMD="swift run homesync-cli" go test -run TestClientConformance ./...
```

The suite then manipulates both sides and checks that changes cross correctly:
creations and edits in both directions, deletions in both directions, nested
directories, ignored files staying put, and a concurrent edit leaving both
versions on disk rather than one of them being lost.

Timeouts are deliberately generous (30s per assertion). A client that batches or
debounces its work is behaving correctly, and a flaky conformance suite is worse
than a slow one.

## On trusting this suite

A test that cannot fail is worse than no test, because it reads like coverage.
The assertions here were checked by breaking the server on purpose and
confirming each one caught it: removing NFC normalisation, moving the
case-collision check to after the write, and making a conflict overwrite instead
of preserving a copy.

That exercise earned its keep. The case-collision assertion originally only
detected the bug on a case-insensitive volume, which meant it proved nothing on
Linux, which is exactly where CI runs it. It now also asserts that a refused
write left nothing behind on disk.
