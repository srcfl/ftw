#!/usr/bin/env bash
# Full local CI pass: Go tests, e2e tests, native/arm64 builds, then a
# browser smoke test against a temporary local stack with simulators.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ARTIFACT_DIR=${FTW_CI_ARTIFACT_DIR:-"$ROOT/artifacts/local-ci/$(date -u +%Y%m%dT%H%M%SZ)"}
MQTT_PORT=${FTW_CI_MQTT_PORT:-11883}
MODBUS_PORT=${FTW_CI_MODBUS_PORT:-15502}
API_PORT=${FTW_CI_API_PORT:-18080}

mkdir -p "$ARTIFACT_DIR"

log() {
  printf '[ci-local] %s\n' "$*"
}

fail() {
  printf '[ci-local] ERROR: %s\n' "$*" >&2
  exit 1
}

PIDS=()
cleanup() {
  if [ "${FTW_CI_KEEP_DEV_SERVER:-0}" = "1" ]; then
    log "leaving local stack running; artifacts: $ARTIFACT_DIR"
    return
  fi
  local pid
  for pid in "${PIDS[@]:-}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
  for pid in "${PIDS[@]:-}"; do
    wait "$pid" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT

wait_for_url() {
  local url=$1
  local out=$2
  local deadline=${3:-45}
  local i

  for i in $(seq 1 "$deadline"); do
    if curl -fsS "$url" > "$out" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  return 1
}

write_config() {
  local path=$1
  cat > "$path" <<YAML
site:
  name: "Local CI"
  control_interval_s: 2
  grid_target_w: 0
  grid_tolerance_w: 42
  watchdog_timeout_s: 60
  smoothing_alpha: 0.3
  gain: 0.5
  slew_rate_w: 500
  min_dispatch_interval_s: 2

fuse:
  max_amps: 16
  phases: 3
  voltage: 230

drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    battery_capacity_wh: 15200
    capabilities:
      mqtt:
        host: 127.0.0.1
        port: $MQTT_PORT

  - name: sungrow
    lua: drivers/sungrow.lua
    battery_capacity_wh: 9600
    capabilities:
      modbus:
        host: 127.0.0.1
        port: $MODBUS_PORT
        unit_id: 1

api:
  port: $API_PORT

state:
  path: "$ARTIFACT_DIR/state.local-ci.db"
  cold_dir: "$ARTIFACT_DIR/cold"

planner:
  enabled: false
YAML
}

log "artifacts: $ARTIFACT_DIR"
log "running Go suite"
(cd "$ROOT" && make test)

log "running full-stack e2e"
(cd "$ROOT" && make e2e)

log "building native binaries"
(cd "$ROOT" && make build)

log "cross-building linux/arm64"
(cd "$ROOT" && make build-arm64)

if [ "${FTW_CI_SKIP_BROWSER:-0}" = "1" ]; then
  log "browser smoke skipped by FTW_CI_SKIP_BROWSER=1"
  exit 0
fi

CONFIG="$ARTIFACT_DIR/config.local-ci.yaml"
write_config "$CONFIG"

log "starting simulators on mqtt:$MQTT_PORT modbus:$MODBUS_PORT"
"$ROOT/bin/sim-ferroamp" -addr ":$MQTT_PORT" > "$ARTIFACT_DIR/sim-ferroamp.log" 2>&1 &
PIDS+=("$!")
"$ROOT/bin/sim-sungrow" -addr "tcp://127.0.0.1:$MODBUS_PORT" > "$ARTIFACT_DIR/sim-sungrow.log" 2>&1 &
PIDS+=("$!")

log "starting app on :$API_PORT"
"$ROOT/bin/forty-two-watts" -config "$CONFIG" -web "$ROOT/web" -drivers "$ROOT/drivers" > "$ARTIFACT_DIR/app.log" 2>&1 &
PIDS+=("$!")

if ! wait_for_url "http://127.0.0.1:$API_PORT/api/health" "$ARTIFACT_DIR/health.wait.json" 60; then
  tail -80 "$ARTIFACT_DIR/app.log" >&2 || true
  fail "app did not become healthy on :$API_PORT"
fi

FTW_CI_ARTIFACT_DIR="$ARTIFACT_DIR/ui" "$ROOT/scripts/ci-ui-browser.sh" "http://127.0.0.1:$API_PORT"

log "ok"
