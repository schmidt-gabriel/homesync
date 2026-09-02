# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first: this layer is cached until go.mod or go.sum changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# What the build calls itself, shown on the admin page. CI passes the release
# tag; a plain `docker compose build` leaves "local", which is the honest
# answer for an image built from a working copy.
ARG VERSION=local

# CGO_ENABLED=0 keeps the binary static — the SQLite driver is pure Go
# (modernc.org/sqlite), so there is nothing to link against.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/schmidt-gabriel/homesync/server/internal/version.Current=${VERSION}" \
    -o /out/homesync ./cmd/homesync


FROM alpine:3.21

# rsync takes the backup snapshots: --link-dest is what makes each dated
# folder a full copy that only stores what changed since the previous one.
RUN apk add --no-cache ca-certificates tzdata wget rsync

# Runs unprivileged. uid 1000 matches the default first user on most Linux
# hosts, so bind-mounted files keep sane ownership.
RUN addgroup -g 1000 homesync && adduser -D -u 1000 -G homesync homesync

COPY --from=build /out/homesync /usr/local/bin/homesync

ENV DATA_DIR=/data \
    CONFIG_DIR=/config \
    LISTEN_ADDR=:8080

RUN mkdir -p /data /config && chown -R homesync:homesync /data /config

USER homesync
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:8080/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/homesync"]
CMD ["serve"]
