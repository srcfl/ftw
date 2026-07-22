# FTW core container — static Go host plus bundled Lua drivers and web assets.
# The optional Python/CVXPY optimizer ships as its own independently updatable
# image from Dockerfile.optimizer. Core falls back safely when it is absent.
#
# Multi-arch: linux/amd64 + linux/arm64 via docker buildx TARGETOS /
# TARGETARCH when available. Plain `docker build` falls back to the
# native Go arch inside the builder image.

# --- Builder ---------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# git is needed by `go build` to resolve VCS info baked into the binary
# via -X main.Version. Everything else is in the base image.
RUN apk add --no-cache git

WORKDIR /src

# Cache the module download as its own layer so source edits don't
# bust the dep cache.
COPY go/go.mod go/go.sum ./go/
RUN cd go && go mod download

COPY go/ ./go/

# Cross-compile by mapping TARGETARCH → GOARCH. CGO is off so the
# binary is fully static and runs on alpine without glibc.
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN cd go && \
    target_arch="${TARGETARCH:-$(go env GOARCH)}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${target_arch} \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/ftw ./cmd/ftw && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${target_arch} \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/ftw-backup ./cmd/ftw-backup
# --- Runtime ---------------------------------------------------------------
FROM alpine:3.22

# HTTPS integrations and timezone-aware price/plan windows need these at
# runtime. BusyBox wget provides the health check without adding Python/curl.
RUN apk add --no-cache ca-certificates tzdata

# Image layout:
#   /app/ftw              binary (immutable, replaced on upgrade)
#   /app/drivers/         bundled Lua drivers (immutable, replaced on upgrade)
#   /app/web/             bundled UI assets (immutable, replaced on upgrade)
#   /app/data/            PERSISTENT — config.yaml, state.db, cold/, models
#
# The container's working directory is /app/data so that any *relative*
# path in the user's config (state.path: state.db, state.cold_dir: cold)
# resolves under the persistent volume by default. Without this, the
# binary would default-write state.db to its CWD and lose every byte
# on container recreate. See go/cmd/ftw/main.go:66 — there
# is no path resolution against the config file's directory; the open
# call is literally state.Open(cfg.State.Path).
COPY --from=builder /out/ftw             /app/ftw
COPY --from=builder /out/ftw-backup      /app/ftw-backup
COPY drivers/ /app/drivers/
COPY web/     /app/web/
COPY LICENSE NOTICE /usr/share/doc/ftw/

RUN ln -s /app/ftw /app/forty-two-watts && \
    mkdir -p /app/data /app/data/drivers /run/ftw-update /run/ftw-optimizer && \
    chown -R 100:101 /app /run/ftw-update /run/ftw-optimizer

ENV HOME=/app/data

USER 100:101
WORKDIR /app/data

VOLUME ["/app/data"]
EXPOSE 8080

# Config + state both live in /app/data — one bind-mount is enough to
# persist everything across upgrades. Drivers and web are absolute
# paths into the immutable image layer so the bundled versions ship
# with each release.
#
# UID note: the process runs as uid 100 / gid 101 for compatibility with
# existing bind mounts. Named docker volumes inherit ownership from the image
# automatically and just work. For HOST BIND MOUNTS, the host
# directory must be owned by uid 100 (or world-writable) before the
# container starts:
#
#     mkdir -p /srv/ftw-data && chown -R 100:101 /srv/ftw-data
#     docker run -v /srv/ftw-data:/app/data ghcr.io/srcfl/ftw:latest
#
# Without this the binary fails fast with "open state … unable to
# open database file" because SQLite can't create state.db inside
# a directory it doesn't own.
# Readiness must stay false until Core has opened and migrated state. The long
# start period lets a valid large migration finish without marking it unhealthy.
HEALTHCHECK --interval=10s --timeout=5s --start-period=30m --retries=12 \
  CMD wget -q -T 4 -O /dev/null http://127.0.0.1:8080/api/status || exit 1

ENTRYPOINT ["/app/ftw"]
CMD ["-config", "/app/data/config.yaml", "-web", "/app/web", "-drivers", "/app/drivers", "-user-drivers", "/app/data/drivers"]
