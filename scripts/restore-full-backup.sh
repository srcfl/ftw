#!/usr/bin/env bash
set -euo pipefail

install_dir=""
archive=""
health_url="${FTW_HEALTH_URL:-http://127.0.0.1:8080/api/health}"
ready_url="${FTW_READY_URL:-${health_url%/api/health}/api/status}"

usage() {
  cat <<'EOF'
Usage: restore-full-backup.sh --archive PATH [--dir INSTALL_DIR]

Verifies a .ftwbak archive, stops only the FTW core service, restores the
persistent data mount, and health-checks the service. If health does not
recover, the pre-restore data is automatically put back.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --archive) archive="${2:-}"; shift 2 ;;
    --dir) install_dir="${2:-}"; shift 2 ;;
    --health-url) health_url="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

[ -n "$archive" ] || { echo "--archive is required" >&2; exit 2; }
[ -f "$archive" ] || { echo "backup archive not found: $archive" >&2; exit 2; }
archive_dir="$(cd "$(dirname "$archive")" && pwd -P)"
archive="$archive_dir/$(basename "$archive")"

if [ -z "$install_dir" ]; then
  install_dir="$PWD"
fi
[ -f "$install_dir/docker-compose.yml" ] || {
  echo "docker-compose.yml not found under $install_dir" >&2
  exit 2
}
cd "$install_dir"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 2; }
docker compose version >/dev/null

services="$(docker compose config --services)"
main_service=""
for candidate in ftw forty-two-watts; do
  if printf '%s\n' "$services" | grep -qx "$candidate"; then
    if [ -n "$main_service" ]; then
      echo "both ftw and forty-two-watts services exist; refusing to guess" >&2
      exit 2
    fi
    main_service="$candidate"
  fi
done
[ -n "$main_service" ] || { echo "no FTW core service found" >&2; exit 2; }

backup_mount="/restore/$(basename "$archive")"
run_backup_tool() {
  docker compose run --rm --no-deps \
    -v "$archive:$backup_mount:ro" \
    --entrypoint /app/ftw-backup \
    "$main_service" "$@"
}

wait_ready() {
  local deadline=$((SECONDS + 1800))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl -fsS --max-time 3 "$health_url" >/dev/null 2>&1 && \
      curl -fsS --max-time 3 "$ready_url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  return 1
}

echo "[FTW restore] verifying archive before stopping FTW"
run_backup_tool verify -archive "$backup_mount" >/dev/null

was_running=false
if [ -n "$(docker compose ps -q --status running "$main_service")" ]; then
  was_running=true
fi
restore_started=false
restore_finished=false
ensure_started() {
  if [ "$was_running" = true ] && [ "$restore_finished" = false ]; then
    docker compose start "$main_service" >/dev/null 2>&1 || true
  fi
}
trap ensure_started EXIT INT TERM

echo "[FTW restore] stopping $main_service"
docker compose stop -t 30 "$main_service"
restore_started=true

echo "[FTW restore] activating verified backup"
restore_json="$(run_backup_tool restore -archive "$backup_mount" -data /app/data -yes)"
safety_dir="$(printf '%s\n' "$restore_json" | sed -n 's/.*"safety_dir": "\([^"]*\)".*/\1/p' | tail -n 1)"
[ -n "$safety_dir" ] || {
  echo "restore completed without returning a safety directory; leaving FTW stopped" >&2
  exit 1
}

echo "[FTW restore] starting $main_service and checking health"
docker compose start "$main_service"
healthy=false
if wait_ready; then healthy=true; fi

if [ "$healthy" = true ]; then
  restore_finished=true
  trap - EXIT INT TERM
  echo "[FTW restore] restore healthy; previous data retained at $safety_dir"
  exit 0
fi

echo "[FTW restore] restored service is unhealthy; reverting previous data" >&2
docker compose stop -t 30 "$main_service" >/dev/null 2>&1 || true
run_backup_tool revert -data /app/data -safety "$safety_dir" -yes >/dev/null
docker compose start "$main_service"

recovered=false
if wait_ready; then recovered=true; fi
restore_finished=true
trap - EXIT INT TERM
if [ "$recovered" = true ]; then
  echo "[FTW restore] backup was rejected; previous installation recovered" >&2
else
  echo "[FTW restore] automatic revert completed but FTW is still unhealthy; inspect docker compose logs" >&2
fi
exit 1
