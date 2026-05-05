#!/usr/bin/env bash
# forty-two-watts one-shot installer.
#
# Supported environments:
#   - Raspberry Pi OS (arm64)
#   - Debian / Ubuntu amd64 (bare-metal or VM)
#   - LXC containers with Docker pre-installed (e.g. Proxmox community-scripts
#     Docker LXC — https://community-scripts.github.io/ProxmoxVE/)
#   - Any Debian/Ubuntu-flavoured Linux with curl + sudo
#
# The pre-built GHCR image is multi-arch (linux/amd64 + linux/arm64), so the
# right variant is pulled automatically for both Pi and x86 hosts.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/scripts/install.sh | bash
#
# What this does:
#   1. Installs Docker Engine + the `docker compose` plugin via
#      get.docker.com (skipped if Docker is already present).
#   2. Adds your user to the `docker` group (skipped when running as root).
#   3. Creates ~/forty-two-watts/ with data/ subdir owned by the
#      in-container ftw user (uid 100 / gid 101).
#   4. Fetches docker-compose.yml from the repo.
#   5. Pulls the multi-arch image from GHCR and starts the container.
#
# Safe to re-run — every step is idempotent, and `docker compose up -d`
# picks up config changes without destroying the state volume.
#
# Override via env vars (optional):
#   FTW_DIR=/srv/ftw       # install location (default: ~/forty-two-watts)
#   FTW_IMAGE=...          # custom image (default: ghcr.io/frahlg/forty-two-watts:latest)
#   FTW_BRANCH=some-branch # pull docker-compose.yml from a non-master branch

set -euo pipefail

# ---- Config (override via env) ----
REPO="frahlg/forty-two-watts"
BRANCH="${FTW_BRANCH:-master}"
IMAGE="${FTW_IMAGE:-ghcr.io/${REPO}:latest}"
INSTALL_DIR="${FTW_DIR:-$HOME/forty-two-watts}"
COMPOSE_URL="${FTW_COMPOSE_URL:-https://raw.githubusercontent.com/${REPO}/${BRANCH}/docker-compose.yml}"

# ---- Environment detection ----
# Use id -un rather than $USER — the latter can be unset in minimal root
# environments (e.g. LXC containers booted without a login shell).
CURRENT_USER="$(id -un)"

# Detect LXC containers: try systemd-detect-virt first (most reliable when
# available), then fall back to scanning /proc/1/environ (set by lxc-start).
IN_LXC=0
if systemd-detect-virt --container 2>/dev/null | grep -q lxc 2>/dev/null; then
  IN_LXC=1
elif grep -qa 'container=lxc' /proc/1/environ 2>/dev/null; then
  IN_LXC=1
fi

# Banner
cat <<'BANNER'

  ┌─────────────────────────────────────────────────┐
  │     forty-two-watts installer                   │
  │     Home Energy Management System               │
  └─────────────────────────────────────────────────┘

BANNER

_arch="$(uname -m)"
_env_note="host (${_arch})"
[ "$IN_LXC" = "1" ] && _env_note="LXC container (${_arch})"
echo "  Detected environment: ${_env_note}"
echo ""

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
# Rootless Docker rarely works inside LXC (kernel user-namespace nesting
# constraints), so we skip this step there to avoid spurious failures.
# On bare-metal / VMs we install it but treat failure as non-fatal.
if ! command -v newuidmap >/dev/null 2>&1; then
  if [ "$IN_LXC" = "1" ]; then
    echo "    Skipping uidmap in LXC (rootless Docker not supported in most LXC setups)."
  else
    echo "    Installing uidmap (needed for rootless Docker)..."
    $SUDO apt-get update -qq
    $SUDO apt-get install -y -qq uidmap 2>/dev/null || \
      echo "    WARNING: uidmap install failed — rootless Docker won't be available, continuing."
  fi
fi

# ---- 2. Docker group ----
echo ""
echo "==[2/5]== Docker group"
if [ "$(id -u)" -eq 0 ]; then
  # Root already has access to the Docker socket; group membership doesn't
  # apply and `usermod -aG docker root` would just produce noise.
  echo "    Running as root — docker group not needed, skipping."
  NEED_RELOGIN=0
elif id -nG "$CURRENT_USER" 2>/dev/null | grep -qw docker; then
  echo "    $CURRENT_USER is already in the docker group — skipping."
  NEED_RELOGIN=0
else
  $SUDO usermod -aG docker "$CURRENT_USER"
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
echo "==[4/5]== Fetching docker-compose.yml from $BRANCH"
curl -fsSL "$COMPOSE_URL" -o "$INSTALL_DIR/docker-compose.yml"

# ---- 5. Pull + start ----
# Run docker as the invoking user, not via sudo. If they were just added
# to the docker group in step 2, their current shell hasn't picked it up
# yet — `sg docker -c ...` executes under the new primary group without
# requiring a re-login. NEED_RELOGIN from step 2 is authoritative here:
# `id -nG` reads /etc/group (already updated by usermod), not the current
# process's credentials, so it can't answer this question.
if [ "$(id -u)" -eq 0 ] || [ "$NEED_RELOGIN" = "0" ]; then
  run_docker() { docker "$@"; }
else
  run_docker() { sg docker -c "docker $*"; }
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
  ✓ forty-two-watts is running.

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
