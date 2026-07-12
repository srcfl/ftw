# forty-two-watts container — static Go host + Python mathematical planner.
#
# Builder uses golang:alpine to keep the Go binary fully static. The runtime is
# Debian slim because the CVXPY and HiGHS ARM64 wheels target glibc.
#
# Multi-arch: linux/amd64 + linux/arm64 via docker buildx TARGETOS /
# TARGETARCH when available. Plain `docker build` falls back to the
# native Go arch inside the builder image.

# --- Builder ---------------------------------------------------------------
FROM golang:1.26-alpine AS builder

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
    -o /out/forty-two-watts ./cmd/forty-two-watts
RUN cd go && \
    target_arch="${TARGETARCH:-$(go env GOARCH)}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${target_arch} \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/ftw-pair ./cmd/ftw-pair

# Note: the standalone relay (ftw-relay) is NOT bundled in this image.
# It is built and deployed separately; this image only includes the main
# service and the local ftw-pair sidecar.
# The cross-platform binaries are published as GitHub release assets
# (ftw-relay-linux-{amd64,arm64}) for operators who self-host.

# --- Optimizer -------------------------------------------------------------
FROM python:3.12-slim-bookworm AS optimizer

COPY optimizer/ /src/optimizer/
RUN python -m venv /opt/venv && \
    /opt/venv/bin/pip install --no-cache-dir /src/optimizer

# --- Runtime ---------------------------------------------------------------
FROM python:3.12-slim-bookworm

# ca-certificates: HTTPS to elprisetjustnu / met.no / ECB FX.
# tzdata: timezone-aware price + plan windows (Europe/Stockholm etc).
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata && \
    rm -rf /var/lib/apt/lists/*

# Image layout:
#   /app/forty-two-watts  binary (immutable, replaced on upgrade)
#   /app/drivers/         bundled Lua drivers (immutable, replaced on upgrade)
#   /app/web/             bundled UI assets (immutable, replaced on upgrade)
#   /app/data/            PERSISTENT — config.yaml, state.db, cold/, models
#
# The container's working directory is /app/data so that any *relative*
# path in the user's config (state.path: state.db, state.cold_dir: cold)
# resolves under the persistent volume by default. Without this, the
# binary would default-write state.db to its CWD and lose every byte
# on container recreate. See go/cmd/forty-two-watts/main.go:66 — there
# is no path resolution against the config file's directory; the open
# call is literally state.Open(cfg.State.Path).
COPY --from=builder /out/forty-two-watts /app/forty-two-watts
COPY --from=builder /out/ftw-pair        /app/ftw-pair
COPY --from=optimizer /opt/venv          /opt/venv
COPY optimizer/                          /app/optimizer/
COPY drivers/ /app/drivers/
COPY web/     /app/web/

RUN mkdir -p /app/data /app/data/drivers /run/ftw-update && \
    chown -R 100:101 /app /run/ftw-update /opt/venv

ENV HOME=/app/data \
    FTW_OPTIMIZER_PYTHON=/opt/venv/bin/python \
    FTW_OPTIMIZER_DIR=/app/optimizer

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
#     docker run -v /srv/ftw-data:/app/data ghcr.io/<owner>/forty-two-watts:latest
#
# Without this the binary fails fast with "open state … unable to
# open database file" because SQLite can't create state.db inside
# a directory it doesn't own.
ENTRYPOINT ["/app/forty-two-watts"]
CMD ["-config", "/app/data/config.yaml", "-web", "/app/web", "-drivers", "/app/drivers", "-user-drivers", "/app/data/drivers"]
