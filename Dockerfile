# syntax=docker/dockerfile:1

# Pinned to the builder's own architecture on purpose. With CGO off the Go
# toolchain cross-compiles for free, so building an arm64 image on an amd64
# runner costs nothing; letting this stage run as arm64 instead would compile
# the whole module under QEMU for no gain.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src

# Dependencies first, so a source-only change reuses this layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
# Supplied by BuildKit. They are empty on a plain `docker build`, which is the
# same as asking for the host's own platform.
ARG TARGETOS TARGETARCH
# CGO stays off: the SQLite driver is pure Go, which is what makes the result a
# single static binary.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/feedrepeater ./cmd/feedrepeater

# Taking the binary from the official image, pinned by digest, avoids a
# build-time download that nothing would verify.
FROM litestream/litestream:0.5.17@sha256:4b02b9859a6b6b4087d8b8944e15f7e984bd7957cba322bbeee38b0e27b9656a AS litestream

FROM alpine:3.24
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 feedrepeater \
 && mkdir -p /data && chown feedrepeater:feedrepeater /data

COPY --from=build /out/feedrepeater /usr/local/bin/feedrepeater
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
COPY deploy/litestream.yml /etc/litestream.yml
# Set the mode here rather than relying on the checkout's file permissions.
COPY --chmod=0755 deploy/entrypoint.sh /usr/local/bin/entrypoint.sh

USER feedrepeater
WORKDIR /data
ENV FR_DB_PATH=/data/feedrepeater.db \
    FR_ADDR=0.0.0.0:8080
EXPOSE 8080
VOLUME ["/data"]

# Every run forks a wget and asks the process for a database probe. The endpoint
# caches its probe, so the cost is now the fork rather than the query, but a
# minute is still the right cadence for "is this container alive" — thirty
# seconds was buying two chances a minute to notice something that a restart
# takes longer than that to fix anyway.
HEALTHCHECK --interval=60s --timeout=3s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
