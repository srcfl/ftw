#!/usr/bin/env bash
# FTW one-shot installer for macOS (Docker Desktop).
#
# The Linux installer (scripts/install.sh) can't run here — macOS has no
# apt-get, no host networking, and Docker Desktop is a GUI app that can't
# be installed unattended from a shell. This script does the rest: it
# verifies Docker Desktop is up, lays out the deploy directory, fetches
# the macOS compose file + broker config, and brings the stack up.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/install-macos.sh | bash
#
# What this does:
#   1. Verifies Docker Desktop is installed and the daemon is running.
#   2. Reuses an existing FTW/legacy install, or creates ~/ftw/data (Docker
#      Desktop maps host ownership transparently, so no chown is needed).
#   3. Fetches docker-compose.macos.yml + mosquitto.conf from the repo.
#   4. Pulls the multi-arch images from GHCR and starts the stack.
#
# Safe to re-run — every step is idempotent, and `docker compose up -d`
# picks up changes without destroying the ./data state.
#
# Override via env vars (optional):
#   FTW_DIR=/path/to/dir   # explicit install location
#   FTW_BRANCH=some-branch # pull files from a non-master branch

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
RAW="https://raw.githubusercontent.com/${REPO}/${BRANCH}"
COMPOSE_FILE="docker-compose.macos.yml"
ENABLE_MODULAR_URL="${RAW}/scripts/enable-modular-stack.sh"

# Banner
cat <<'BANNER'

  ┌─────────────────────────────────────────────────┐
  │     FTW installer (macOS)                       │
  │     Local-first home energy coordination.       │
  └─────────────────────────────────────────────────┘

BANNER

# ---- Platform guard ----
if [ "$(uname -s)" != "Darwin" ]; then
  echo "ERROR: this script is for macOS. On Linux, use scripts/install.sh." >&2
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "ERROR: 'curl' is required (it ships with macOS — odd that it's missing)." >&2
  exit 1
fi

# ---- 1. Docker Desktop ----
echo "==[1/4]== Checking Docker Desktop"
if ! command -v docker >/dev/null 2>&1; then
  cat >&2 <<'EOF'
    Docker is not installed.

    Install Docker Desktop for Mac (Apple Silicon or Intel — the images
    are multi-arch, so either works), start it, then re-run this script:

        https://docs.docker.com/desktop/install/mac-install/

    Or via Homebrew:  brew install --cask docker
EOF
  exit 1
fi

# `docker info` fails fast if the daemon (the Docker Desktop VM) isn't up.
if ! docker info >/dev/null 2>&1; then
  cat >&2 <<'EOF'
    Docker is installed but the daemon isn't running.

    Open Docker Desktop (from Applications or Spotlight), wait for the
    whale icon in the menu bar to stop animating, then re-run this script.
EOF
  exit 1
fi

if ! docker compose version >/dev/null 2>&1; then
  echo "ERROR: the 'docker compose' plugin is missing. Update Docker Desktop." >&2
  exit 1
fi
echo "    Docker Desktop is running."

# ---- 2. Install directory ----
echo ""
echo "==[2/4]== Preparing install directory: $INSTALL_DIR"
# No chown to uid 100:101 here, unlike the Linux installer: Docker
# Desktop's file sharing maps host ownership transparently, so the
# container's ftw user can always write to ./data.
mkdir -p "$INSTALL_DIR/data" "$INSTALL_DIR/mosquitto/config"

# ---- 3. Compose file + broker config ----
echo ""
echo "==[3/4]== Preparing $COMPOSE_FILE + mosquitto.conf from $BRANCH"
COMPOSE_PATH="$INSTALL_DIR/${COMPOSE_FILE}"

refresh_compose_args() {
  COMPOSE_ARGS=(-f "$COMPOSE_PATH")
  for name in \
    docker-compose.override.yml \
    docker-compose.override.yaml \
    compose.override.yml \
    compose.override.yaml; do
    candidate="$INSTALL_DIR/$name"
    if [ -e "$candidate" ]; then
      COMPOSE_ARGS+=(-f "$candidate")
    fi
  done
}

compose() {
  docker compose "${COMPOSE_ARGS[@]}" "$@"
}

if [ -f "$COMPOSE_PATH" ]; then
  refresh_compose_args
  SERVICES="$(compose config --services)"
  MAIN_COUNT="$(printf '%s\n' "$SERVICES" | grep -Ec '^(ftw|forty-two-watts)$' || true)"
  if [ "$MAIN_COUNT" -ne 1 ]; then
    echo "ERROR: existing compose layout is ambiguous or lacks /app/data; leaving it untouched." >&2
    exit 1
  fi
  MAIN_SERVICE="$(printf '%s\n' "$SERVICES" | grep -E '^(ftw|forty-two-watts)$')"
  if ! compose config "$MAIN_SERVICE" | grep -q '/app/data'; then
    echo "ERROR: existing main service lacks /app/data; leaving it untouched." >&2
    exit 1
  fi
  cp "$COMPOSE_PATH" "$COMPOSE_PATH.pre-ftw.bak"
  echo "    Existing safe deployment layout retained."

  if ! printf '%s\n' "$SERVICES" | grep -qx 'ftw-optimizer'; then
    echo "    Adding the independently updatable optimizer sidecar..."
    ENABLE_SCRIPT="$(mktemp)"
    trap 'rm -f "$ENABLE_SCRIPT"' EXIT
    curl -fsSL "$ENABLE_MODULAR_URL" -o "$ENABLE_SCRIPT"
    if ! bash "$ENABLE_SCRIPT" "$COMPOSE_PATH"; then
      echo "ERROR: could not add the modular optimizer without changing an existing override." >&2
      echo "       Merge it manually using docs/operations.md, then rerun this installer." >&2
      exit 1
    fi
    rm -f "$ENABLE_SCRIPT"
    trap - EXIT
    refresh_compose_args
  fi
else
  curl -fsSL "${RAW}/${COMPOSE_FILE}" -o "$COMPOSE_PATH"
  refresh_compose_args
fi
curl -fsSL "${RAW}/mosquitto/config/mosquitto.conf" \
  -o "$INSTALL_DIR/mosquitto/config/mosquitto.conf"

# ---- 4. Pull + start ----
echo ""
echo "==[4/4]== Pulling images + starting the stack"
cd "$INSTALL_DIR"
compose pull
compose up -d

COMPOSE_MANAGE="docker compose -f $COMPOSE_FILE"
for name in \
  docker-compose.override.yml \
  docker-compose.override.yaml \
  compose.override.yml \
  compose.override.yaml; do
  if [ -e "$INSTALL_DIR/$name" ]; then
    COMPOSE_MANAGE="$COMPOSE_MANAGE -f $name"
  fi
done

# ---- Summary ----
# en0 is Wi-Fi on most Macs; en1 is the wired port on a Mac mini. Try a
# couple before falling back to localhost.
HOST_IP=""
for IFACE in en0 en1 en2; do
  HOST_IP="$(ipconfig getifaddr "$IFACE" 2>/dev/null || true)"
  [ -n "$HOST_IP" ] && break
done
[ -z "$HOST_IP" ] && HOST_IP="localhost"

cat <<EOF

──────────────────────────────────────────────────────────────────
  ✓ FTW is running.

  Open the dashboard:
     http://${HOST_IP}:8080/         (from another device on the LAN)
     http://localhost:8080/          (from this Mac)

  First-time setup wizard:
     http://localhost:8080/setup

  Install directory:    ${INSTALL_DIR}
  Persistent data:      ${INSTALL_DIR}/data/
    └── config.yaml, state.db, battery models, cold/ rolloff

  Manage the stack (from ${INSTALL_DIR}):
     ${COMPOSE_MANAGE} logs -f                    # tail logs
     ${COMPOSE_MANAGE} pull && ${COMPOSE_MANAGE} up -d  # upgrade
     ${COMPOSE_MANAGE} down                       # stop

  macOS networking notes (see docs/deploy-platforms.md):
     • In config.yaml, point MQTT drivers at host: mosquitto (NOT localhost).
     • Give every driver an explicit IP — mDNS (zap.local) and broadcast
       discovery do not cross the Docker Desktop VM boundary.

──────────────────────────────────────────────────────────────────
EOF
