# All-in-one Home Assistant add-on image for FTW.
#
# FTW normally runs as several containers (docker-compose.yml): the Go core,
# the Python/CVXPY optimizer, an updater sidecar and (optionally) Mosquitto.
# A Home Assistant add-on is single-container, so this image bundles the two
# that matter for the app itself — CORE + OPTIMIZER — into one. They already
# communicate over a Unix socket (/run/ftw-optimizer/optimizer.sock), so
# inside one container they simply share that path; there is no networking
# between them and nothing to coordinate.
#
# Dropped on purpose:
#   - ftw-updater  — Supervisor owns add-on updates.
#   - mosquitto    — use the HA Mosquitto add-on, or any LAN broker.
#
# Base = the optimizer image (Debian `python:3.12-slim` + the CVXPY venv at
# /opt/venv). The core binary is a fully static, CGO-free Go build, so it runs
# on this Debian base unchanged. build.yaml pins BOTH images per release; they
# version independently (e.g. core v1.4.0 + optimizer v1.3.1).

ARG BUILD_FROM                                   # ghcr.io/srcfl/ftw-optimizer:<tag>
ARG FTW_CORE_FROM=ghcr.io/srcfl/ftw:v1.4.0

FROM ${FTW_CORE_FROM} AS core

# hadolint ignore=DL3006
FROM ${BUILD_FROM}
USER root

# Core needs TLS roots (price/weather HTTPS) and tzdata (tz-aware windows).
# tini is PID 1 so the two long-running children get reaped cleanly.
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata tini \
 && rm -rf /var/lib/apt/lists/*

# Core artifacts from the Go image: the static binary + bundled web + drivers.
COPY --from=core /app/ftw     /app/ftw
COPY --from=core /app/web     /app/web
COPY --from=core /app/drivers /app/drivers

# Supervisor's persistent volume + the shared optimizer socket directory.
RUN mkdir -p /data/drivers /run/ftw-optimizer
WORKDIR /data

# transport=auto: core dials the bundled optimizer over the socket below, and
# falls back to the in-Go DP planner if it is ever unavailable.
ENV FTW_OPTIMIZER_TRANSPORT=auto \
    FTW_OPTIMIZER_SOCKET=/run/ftw-optimizer/optimizer.sock

COPY run.sh /run.sh
RUN chmod +x /run.sh

# Health = the user-facing service (core API on :8080), not the optimizer.
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=6 \
  CMD python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/api/health', timeout=4)" || exit 1

ENTRYPOINT ["/usr/bin/tini", "--", "/run.sh"]
