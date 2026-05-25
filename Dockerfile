# forty-two-watts container — multi-stage Go build, alpine runtime.
#
# Builder uses golang:alpine to keep the final binary fully static and
# the toolchain layer small. The runtime stage is plain alpine with
# only ca-certificates + tzdata; everything else (Lua drivers, web
# assets, the binary itself) ships verbatim.
#
# Multi-arch: linux/amd64 + linux/arm64 via docker buildx BUILDPLATFORM
# / TARGETPLATFORM split so the toolchain runs natively on the build
# host and only `go build` is cross-compiled.

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
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN cd go && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/forty-two-watts ./cmd/forty-two-watts
RUN cd go && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/ftw-pair ./cmd/ftw-pair

# --- Runtime ---------------------------------------------------------------
FROM alpine:3.20

# ca-certificates: HTTPS to elprisetjustnu / met.no / ECB FX.
# tzdata:        timezone-aware price + plan windows (Europe/Stockholm etc).
# python3 + pipx: needed by fowl (magic-wormhole transport for `pair`).
# libsodium:      NaCl crypto used by fowl/twisted/wormhole at runtime.
RUN apk add --no-cache ca-certificates tzdata python3 pipx libsodium && \
    addgroup -S ftw && adduser -S ftw -G ftw

# Install fowl into a system-wide location so it's on PATH for everyone.
# PIPX_HOME=/opt/pipx keeps it outside the ftw home dir; PIPX_BIN_DIR
# puts the entry-point wrappers on PATH without any per-user activation.
# We run this before switching to USER ftw so the install lands in system
# paths and is readable by all users (including root for debugging).
ENV PIPX_HOME=/opt/pipx
ENV PIPX_BIN_DIR=/usr/local/bin
RUN PIPX_HOME=/opt/pipx PIPX_BIN_DIR=/usr/local/bin pipx install fowl && \
    chown -R ftw:ftw /opt/pipx

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
COPY drivers/ /app/drivers/
COPY web/     /app/web/

RUN mkdir -p /app/data /run/ftw-update && \
    chown -R ftw:ftw /app /run/ftw-update

USER ftw
WORKDIR /app/data

VOLUME ["/app/data"]
EXPOSE 8080

# Config + state both live in /app/data — one bind-mount is enough to
# persist everything across upgrades. Drivers and web are absolute
# paths into the immutable image layer so the bundled versions ship
# with each release.
#
# UID note: the ftw user is uid 100 / gid 101 (alpine `adduser -S`
# convention). Named docker volumes inherit ownership from the image
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
CMD ["-config", "/app/data/config.yaml", "-web", "/app/web", "-drivers", "/app/drivers"]
