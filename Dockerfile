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

# Cross-compile by mapping TARGETARCH → GOARCH. CGO stays off: the binary is
# fully static, so it is the runtime's *userland* we are choosing below, not a
# libc the binary depends on. Keeping CGO off is what lets the toolchain run
# natively on the build platform instead of under emulation.
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
# Debian trixie-slim — current Debian stable (13), and the same suite as
# Dockerfile.updater and Dockerfile.optimizer's python:3.12-slim-trixie. One
# rootfs blob is pulled once and shared by all three images, so the extra bytes
# over alpine are paid a single time per host rather than per image, and there
# is one libc and one security stream to track. It also matches the Raspberry Pi
# OS release the SD image is built from (deploy/pi-gen/config: RELEASE=trixie).
#
# Pinned to the codename, not `stable-slim`: a suite alias would silently jump
# major versions on some future rebuild. The `debian base currency` workflow
# watches for a new stable and files an issue, so the bump stays deliberate.
#
# glibc also means the image can run ordinary prebuilt vendor binaries, which
# musl cannot, and ships a full userland for on-site debugging.
FROM debian:trixie-slim

# ca-certificates  — HTTPS integrations.
# tzdata           — timezone-aware price/plan windows. Without a zoneinfo tree
#                    time.Local silently degrades to UTC and mis-times plan
#                    boundaries with no error, so this is load-bearing.
# wget             — the HEALTHCHECK below AND ftw-updater's readiness probe,
#                    which `docker exec`s wget in THIS image to decide whether
#                    an update commits. Debian slim does not include wget by
#                    default, so it must be installed explicitly; dropping it
#                    would make every self-update fail its health gate and roll
#                    back.
# libnss-mdns      — resolves ".local" for glibc programs in the image (getent,
#                    wget), so in-container debugging agrees with the
#                    host. apt wires mdns4_minimal into /etc/nsswitch.conf on
#                    install. At run time it forwards to avahi-daemon over
#                    /run/avahi-daemon/socket, which must be bind-mounted; see
#                    docs/operations.md. It does nothing for the FTW binary
#                    itself, which is CGO_ENABLED=0 and therefore never consults
#                    NSS — see the note on the builder stage above.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ca-certificates tzdata wget libnss-mdns && \
    rm -rf /var/lib/apt/lists/*

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
COPY --from=builder --chown=100:101 /out/ftw        /app/ftw
COPY --from=builder --chown=100:101 /out/ftw-backup /app/ftw-backup
COPY --chown=100:101 drivers/ /app/drivers/
COPY --chown=100:101 web/     /app/web/
COPY LICENSE NOTICE /usr/share/doc/ftw/

RUN ln -s /app/ftw /app/forty-two-watts && \
    mkdir -p /app/data /app/data/drivers /run/ftw-update /run/ftw-optimizer && \
    chown 100:101 /app/data /app/data/drivers /run/ftw-update /run/ftw-optimizer

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
# existing bind mounts. These are deliberately NUMERIC — no account is created
# and none is needed, which is why ENV HOME above is load-bearing. Verified on
# this base: uid 100 and gid 101 have no passwd/group entry, so ownership simply
# renders numerically. Do not renumber: gid 101 is what grants access to the
# optimizer's 0660 socket, and existing installs (and every flashed SD card)
# already own their data dir as 100:101.
# Named docker volumes inherit ownership from the image
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
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s --retries=12 \
  CMD wget -q -T 4 -O /dev/null http://127.0.0.1:8080/api/health || exit 1

ENTRYPOINT ["/app/ftw"]
CMD ["-config", "/app/data/config.yaml", "-web", "/app/web", "-drivers", "/app/drivers", "-user-drivers", "/app/data/drivers"]
