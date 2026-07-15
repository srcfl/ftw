#!/usr/bin/env bash
# FTW one-shot installer.
#
# Designed for a freshly-installed Raspberry Pi OS (arm64) but works on
# any Debian/Ubuntu-flavoured Linux with curl + sudo.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/install.sh | bash
#
# What this does:
#   1. Installs Docker Engine + the `docker compose` plugin via
#      get.docker.com (skipped if Docker is already present).
#   2. Adds your user to the `docker` group.
#   3. Reuses an existing FTW/legacy install, or creates ~/ftw, with data/
#      in-container ftw user (uid 100 / gid 101).
#   4. Fetches docker-compose.yml from the repo.
#   5. Pulls the multi-arch image from GHCR and starts the container.
#
# Safe to re-run — every step is idempotent, and `docker compose up -d`
# picks up config changes without destroying the state volume.
#
# Override via env vars (optional):
#   FTW_DIR=/srv/ftw       # explicit install location
#   FTW_BRANCH=some-branch # pull docker-compose.yml from a non-master branch

set -euo pipefail

# ---- Config (override via env) ----
REPO="srcfl/ftw"
BRANCH="${FTW_BRANCH:-master}"
if [ -n "${FTW_DIR:-}" ]; then
  INSTALL_DIR="$FTW_DIR"
elif [ -d "$HOME/ftw" ]; then
  INSTALL_DIR="$HOME/ftw"
elif [ -d "$HOME/forty-two-watts" ]; then
  INSTALL_DIR="$HOME/forty-two-watts"
else
  INSTALL_DIR="$HOME/ftw"
fi
COMPOSE_URL="${FTW_COMPOSE_URL:-https://raw.githubusercontent.com/${REPO}/${BRANCH}/docker-compose.yml}"

# Banner
cat <<'BANNER'

  ┌─────────────────────────────────────────────────┐
  │     FTW installer                               │
  │     Local-first home energy coordination.       │
  └─────────────────────────────────────────────────┘

BANNER

# ---- Platform guard ----
# This installer is Linux-only (apt-get, get.docker.com, `hostname -I`,
# usermod, host networking). On macOS the deploy story is different —
# Docker Desktop + the dedicated macOS compose file. Bail early with a
# pointer instead of failing halfway through with cryptic errors.
if [ "$(uname -s)" = "Darwin" ]; then
  cat >&2 <<'EOF'
This installer is for Linux only.

On macOS, install Docker Desktop and use docker-compose.macos.yml:

  mkdir -p ~/ftw/data && cd ~/ftw
  curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/docker-compose.macos.yml -o docker-compose.macos.yml
  docker compose -f docker-compose.macos.yml up -d

Full walkthrough: docs/deploy-platforms.md
EOF
  exit 1
fi

# ---- Prerequisites ----
if ! command -v curl >/dev/null 2>&1; then
  echo "ERROR: 'curl' is required. Install with:" >&2
  echo "       sudo apt-get update && sudo apt-get install -y curl" >&2
  exit 1
fi

# Docker install + chown need root. Prime sudo up-front so we don't
# interrupt the flow mid-way with a password prompt that the user
# might miss when the script is piped from curl.
if [ "$(id -u)" -eq 0 ]; then
  SUDO=""
else
  if ! command -v sudo >/dev/null 2>&1; then
    echo "ERROR: 'sudo' is required when not running as root." >&2
    exit 1
  fi
  SUDO="sudo"
  echo "This installer needs sudo to install Docker. You may be prompted for your password."
  echo ""
  sudo -v
fi

# ---- 1. Docker ----
echo "==[1/5]== Installing Docker Engine + compose plugin"
if command -v docker >/dev/null 2>&1; then
  echo "    Docker is already installed — skipping."
else
  curl -fsSL https://get.docker.com | $SUDO sh
fi

# get.docker.com ships the compose plugin on current Debian/Raspbian,
# but older systems may need it installed separately.
if ! $SUDO docker compose version >/dev/null 2>&1; then
  echo "    Installing docker-compose-plugin separately..."
  $SUDO apt-get update -qq
  $SUDO apt-get install -y -qq docker-compose-plugin
fi

# uidmap (newuidmap/newgidmap) is required if the user later wants to
# switch to rootless Docker via `dockerd-rootless-setuptool.sh install`.
# Tiny package, harmless to have, saves a confusing second-step detour.
if ! command -v newuidmap >/dev/null 2>&1; then
  echo "    Installing uidmap (needed for rootless Docker)..."
  $SUDO apt-get update -qq
  $SUDO apt-get install -y -qq uidmap
fi

# ---- 2. Docker group ----
echo ""
echo "==[2/5]== Adding $USER to the docker group"
if id -nG "$USER" 2>/dev/null | grep -qw docker; then
  echo "    $USER is already in the docker group — skipping."
  NEED_RELOGIN=0
else
  $SUDO usermod -aG docker "$USER"
  echo "    Done. You'll need to run 'newgrp docker' or log out + back in"
  echo "    before 'docker' works without sudo in your shell."
  NEED_RELOGIN=1
fi

# ---- 3. Install directory ----
echo ""
echo "==[3/5]== Preparing install directory: $INSTALL_DIR"
mkdir -p "$INSTALL_DIR/data"
# The image runs as uid 100 / gid 101 (the `ftw` user created in the
# alpine runtime stage — see Dockerfile). A bind-mounted host dir must
# match those IDs so SQLite can create state.db inside it.
$SUDO chown -R 100:101 "$INSTALL_DIR/data"

# ---- 4. docker-compose.yml ----
echo ""
echo "==[4/5]== Preparing docker-compose.yml from $BRANCH"
COMPOSE_PATH="$INSTALL_DIR/docker-compose.yml"
if [ -f "$COMPOSE_PATH" ]; then
  # Existing deployments keep their service, project, bind path, and image
  # namespace. The updater supports both layouts and legacy images remain
  # published during the compatibility window.
  SERVICES="$($SUDO docker compose -f "$COMPOSE_PATH" config --services)"
  MAIN_COUNT="$(printf '%s\n' "$SERVICES" | grep -Ec '^(ftw|forty-two-watts)$' || true)"
  if [ "$MAIN_COUNT" -ne 1 ]; then
    echo "ERROR: existing compose must contain exactly one ftw/forty-two-watts service; leaving it untouched." >&2
    exit 1
  fi
  MAIN_SERVICE="$(printf '%s\n' "$SERVICES" | grep -E '^(ftw|forty-two-watts)$')"
  if ! $SUDO docker compose -f "$COMPOSE_PATH" config "$MAIN_SERVICE" | grep -q '/app/data'; then
    echo "ERROR: existing main service has no visible /app/data mount; leaving it untouched." >&2
    exit 1
  fi
  cp "$COMPOSE_PATH" "$COMPOSE_PATH.pre-ftw.bak"
  echo "    Existing safe deployment layout retained (backup: docker-compose.yml.pre-ftw.bak)."
else
  curl -fsSL "$COMPOSE_URL" -o "$COMPOSE_PATH"
fi

# ---- 5. Pull + start ----
# Run Docker as the invoking user when its current shell already has access.
# If the user was just added to the docker group, use sudo for this first run;
# the next login picks up group membership. NEED_RELOGIN from step 2 is authoritative:
# `id -nG "$USER"` reads /etc/group (already updated by usermod), not the
# current process's credentials, so it can't answer this question.
if [ "$(id -u)" -eq 0 ] || [ "$NEED_RELOGIN" = "0" ]; then
  run_docker() { docker "$@"; }
else
  run_docker() { sudo docker "$@"; }
fi

echo ""
echo "==[5/5]== Pulling image + starting container"
cd "$INSTALL_DIR"
run_docker compose pull
run_docker compose up -d

# ---- Summary ----
HOST_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -z "$HOST_IP" ] && HOST_IP="localhost"

cat <<EOF

──────────────────────────────────────────────────────────────────
  ✓ FTW is running.

  Open the dashboard:
     http://${HOST_IP}:8080/         (from another device on the LAN)
     http://localhost:8080/          (from this machine)

  First-time setup wizard:
     http://${HOST_IP}:8080/setup

  Install directory:    ${INSTALL_DIR}
  Persistent data:      ${INSTALL_DIR}/data/
    └── config.yaml, state.db, battery models, cold/ rolloff

  Manage the container (from ${INSTALL_DIR}):
     docker compose logs -f                      # tail logs
     docker compose pull && docker compose up -d # upgrade
     docker compose down                         # stop

EOF

if [ "$NEED_RELOGIN" = "1" ]; then
  cat <<'EOF'
  NOTE: your current shell isn't in the docker group yet. Until you log
        out + back in (or run 'newgrp docker'), prefix docker commands
        with sudo, e.g.  `sudo docker compose logs -f`.

EOF
fi

echo "──────────────────────────────────────────────────────────────────"
