#!/usr/bin/env bash
# Deploy the current candidate to a Raspberry Pi CI slot and browser-smoke it.
#
# The Pi instance is intentionally a UI/proxy candidate:
# - candidate binary + web assets run on FTW_PI_PORT, default 18080
# - config has no drivers, so it does not own hardware/control
# - /api/* is proxied read-only to FTW_PI_UPSTREAM, default http://127.0.0.1:8080
#
# Usage:
#   scripts/ci-hw-pi.sh [ssh-host]
#
# Defaults:
#   FTW_PI_HOST=fredde@192.168.192.40
#   FTW_PI_DIR=forty-two-watts-ci
#   FTW_PI_PORT=18080
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
HOST=${1:-${FTW_PI_HOST:-fredde@192.168.192.40}}
REMOTE_DIR=${FTW_PI_DIR:-forty-two-watts-ci}
PI_PORT=${FTW_PI_PORT:-18080}
UPSTREAM=${FTW_PI_UPSTREAM:-http://127.0.0.1:8080}
ARTIFACT_DIR=${FTW_CI_ARTIFACT_DIR:-"$ROOT/artifacts/hw-ci/$(date -u +%Y%m%dT%H%M%SZ)"}

if [[ "$REMOTE_DIR" == *" "* || "$REMOTE_DIR" == *"'"* ]]; then
  printf '[ci-hw] ERROR: FTW_PI_DIR must not contain spaces or single quotes\n' >&2
  exit 2
fi
if [[ "$UPSTREAM" == *"'"* ]]; then
  printf '[ci-hw] ERROR: FTW_PI_UPSTREAM must not contain single quotes\n' >&2
  exit 2
fi

mkdir -p "$ARTIFACT_DIR"

log() {
  printf '[ci-hw] %s\n' "$*"
}

fail() {
  printf '[ci-hw] ERROR: %s\n' "$*" >&2
  exit 1
}

write_config() {
  local path=$1
  cat > "$path" <<YAML
site:
  name: "Pi CI candidate"
  control_interval_s: 30
  grid_target_w: 0
  grid_tolerance_w: 42
  watchdog_timeout_s: 60
  smoothing_alpha: 0.3
  gain: 0.5
  slew_rate_w: 500
  min_dispatch_interval_s: 30

fuse:
  max_amps: 16
  phases: 3
  voltage: 230

# Deliberately empty: this candidate serves the new UI/static assets and
# read-only proxies /api/* to the live instance. It must not open hardware
# sessions or send battery/curtailment/loadpoint commands.
drivers: []

api:
  port: $PI_PORT

state:
  path: "state.hw-ci.db"
  cold_dir: "cold.hw-ci"

homeassistant:
  enabled: false

planner:
  enabled: false
YAML
}

wait_remote_health() {
  local i
  for i in $(seq 1 45); do
    if ssh "$HOST" "curl -fsS http://127.0.0.1:$PI_PORT/api/health" > "$ARTIFACT_DIR/remote-health.json" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

if [ "${FTW_SKIP_LOCAL:-0}" != "1" ]; then
  log "running local tests before Pi deploy"
  (cd "$ROOT" && make test)
  (cd "$ROOT" && make e2e)
fi

log "building linux/arm64 candidate"
(cd "$ROOT" && make build-arm64)

CONFIG="$ARTIFACT_DIR/config.hw-ci.yaml"
write_config "$CONFIG"

log "deploying to $HOST:~/$REMOTE_DIR"
ssh "$HOST" "mkdir -p '$REMOTE_DIR'"
rsync -az --delete "$ROOT/web/" "$HOST:$REMOTE_DIR/web/"
rsync -az --delete "$ROOT/drivers/" "$HOST:$REMOTE_DIR/drivers/"
rsync -az "$ROOT/bin/forty-two-watts-linux-arm64" "$HOST:$REMOTE_DIR/forty-two-watts.new"
rsync -az "$CONFIG" "$HOST:$REMOTE_DIR/config.hw-ci.yaml"

log "starting candidate on port $PI_PORT with read-only upstream $UPSTREAM"
ssh "$HOST" "
  set -eu
  cd '$REMOTE_DIR'
  if [ -f ci.pid ]; then
    old=\$(cat ci.pid || true)
    if [ -n \"\$old\" ] && kill -0 \"\$old\" 2>/dev/null; then
      cmd=\$(ps -p \"\$old\" -o args= 2>/dev/null || true)
      case \"\$cmd\" in
        *config.hw-ci.yaml*) kill \"\$old\" 2>/dev/null || true; sleep 1 ;;
      esac
    fi
  fi
  mv forty-two-watts.new forty-two-watts
  chmod +x forty-two-watts
  : > ci.log
  FTW_PROXY_UPSTREAM='$UPSTREAM' FTW_PROXY_READONLY=1 \\
    nohup ./forty-two-watts -config config.hw-ci.yaml -web web -drivers drivers >> ci.log 2>&1 &
  echo \$! > ci.pid
"

if ! wait_remote_health; then
  ssh "$HOST" "cd '$REMOTE_DIR' && tail -120 ci.log" >&2 || true
  fail "candidate did not become healthy on the Pi"
fi

target_host=${HOST#*@}
target_host=${target_host%%:*}
HTTP_HOST=${FTW_PI_HTTP_HOST:-$target_host}
BASE_URL=${FTW_PI_BASE_URL:-http://$HTTP_HOST:$PI_PORT}

if [ "${FTW_CI_SKIP_BROWSER:-0}" = "1" ]; then
  log "browser smoke skipped by FTW_CI_SKIP_BROWSER=1"
else
  FTW_CI_ARTIFACT_DIR="$ARTIFACT_DIR/ui" "$ROOT/scripts/ci-ui-browser.sh" "$BASE_URL"
fi

log "candidate is still running: $BASE_URL"
log "remote log: ssh $HOST 'tail -f ~/$REMOTE_DIR/ci.log'"
log "ok"
