#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/upgrade-paired-release.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

bash -n "$SCRIPT"

expect_fail() {
  local name="$1"
  shift
  if "$@" >/dev/null 2>"$TMP/err"; then
    echo "expected failure: $name" >&2
    exit 1
  fi
}

expect_fail "missing tag" bash "$SCRIPT" --dir "$TMP/missing"
expect_fail "latest alias" bash "$SCRIPT" --tag latest --dir "$TMP/missing"
expect_fail "moving beta alias" bash "$SCRIPT" --tag beta --dir "$TMP/missing"
expect_fail "known unsafe updater" bash "$SCRIPT" --tag v3.2.0-beta.1 --dir "$TMP/missing"
grep -q 'DuckDB updater fix' "$TMP/err"

mkdir -p "$TMP/bin" "$TMP/site/data" "$TMP/site/ftw-backups" "$TMP/state"
: >"$TMP/site/data/state.db"
cat >"$TMP/site/docker-compose.yml" <<'YAML'
services:
  ftw:
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}
    environment:
      FTW_IMAGE_TAG: ${FTW_IMAGE_TAG:-}
    volumes:
      - ./data:/app/data
  ftw-updater:
    image: ghcr.io/srcfl/ftw-updater:${FTW_UPDATER_IMAGE_TAG:-latest}
YAML
printf 'FTW_IMAGE_TAG=v2.14.1-beta.1\n# keep me\nFTW_API_TOKEN=secret\n' >"$TMP/site/.env"
chmod 600 "$TMP/site/.env"

cat >"$TMP/bin/uname" <<'EOF'
#!/usr/bin/env bash
echo Linux
EOF

cat >"$TMP/bin/curl" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat >"$TMP/bin/chmod" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat >"$TMP/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${FAKE_STATE_DIR:?}"
data="${FAKE_DATA_DIR:?}"
log="${DOCKER_LOG:?}"
target="${FAKE_TARGET_TAG:?}"
caps="${FAKE_CAPS_JSON:-}"

record() {
  printf '%s\n' "$*" >>"$log"
}

image_for() {
  local service="$1"
  if [ -f "$state/${service}.image" ]; then
    cat "$state/${service}.image"
  elif [ "$service" = ftw-updater ]; then
    printf 'ghcr.io/srcfl/ftw-updater:v2.14.1-beta.1\n'
  else
    printf 'ghcr.io/srcfl/ftw:v2.14.1-beta.1\n'
  fi
}

set_image() {
  printf '%s\n' "$2" >"$state/${1}.image"
}

if [ "${1:-}" = info ]; then
  exit 0
fi
if [ "${1:-}" = pull ]; then
  record "docker $*"
  exit 0
fi
if [ "${1:-}" = inspect ]; then
  id="${2:-}"
  format=""
  shift 2 || true
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --format) format="$2"; shift 2 ;;
      *) shift ;;
    esac
  done
  case "$id" in
    ftw|core-id)
      case "$format" in
        *com.docker.compose.project*) printf 'ftw\n' ;;
        *Config.Image*) image_for ftw ;;
        *'/run/ftw-update'*) printf 'ftw_update-ipc\n' ;;
        *) printf '\n' ;;
      esac
      ;;
    ftw-updater|updater-id)
      case "$format" in
        *com.docker.compose.project*) printf 'ftw\n' ;;
        *Config.Image*) image_for ftw-updater ;;
        *'/run/ftw-update'*) printf 'ftw_update-ipc\n' ;;
        *) printf '\n' ;;
      esac
      ;;
    *)
      exit 1
      ;;
  esac
  exit 0
fi
if [ "${1:-}" = container ]; then
  [ "${2:-}" = inspect ] || exit 1
  docker inspect "${3:-}" >/dev/null
  exit 0
fi
if [ "${1:-}" = ps ]; then
  exit 0
fi
if [ "${1:-}" = run ]; then
  record "docker $*"
  shift
  entrypoint=""
  backup_mount=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --rm|--network|--user)
        if [ "$1" = --user ]; then
          shift 2
        elif [ "$1" = --network ]; then
          shift 2
        else
          shift
        fi
        ;;
      --entrypoint)
        entrypoint="${2:-}"
        shift 2
        ;;
      -v)
        mount="${2:-}"
        case "$mount" in
          *:/backup) backup_mount="${mount%:/backup}" ;;
        esac
        shift 2
        ;;
      *)
        shift
        break
        ;;
    esac
  done
  if [ "$entrypoint" = /app/ftw-backup ]; then
    case "${1:-}" in
      create)
        mkdir -p "$backup_mount"
        : >"$backup_mount/ftw-full-backup-test.ftwbak"
        printf '{\n  "id": "ftw-full-backup-test.ftwbak",\n  "verified": true\n}\n'
        ;;
      verify)
        test -f "$backup_mount/ftw-full-backup-test.ftwbak"
        ;;
      *) exit 1 ;;
    esac
    exit 0
  fi
  if [ "$entrypoint" = chown ]; then
    exit 0
  fi
  if [ "$entrypoint" = wget ]; then
    [ -n "$caps" ] || exit 1
    printf '%s\n' "$caps"
    exit 0
  fi
  exit 1
fi
if [ "${1:-}" != compose ]; then
  exit 1
fi
shift
record "compose $*"
while [ "${1:-}" = --project-name ]; do
  shift 2
done
case "${1:-}" in
  version)
    echo "Docker Compose version 2.0.0"
    ;;
  config)
    shift
    if [ "${1:-}" = --services ]; then
      printf 'ftw\nftw-updater\n'
      exit 0
    fi
    if [ "${1:-}" = --images ]; then
      printf 'ghcr.io/srcfl/ftw:%s\n' "${FTW_IMAGE_TAG:-v2.14.1-beta.1}"
      printf 'ghcr.io/srcfl/ftw-updater:%s\n' "${FTW_UPDATER_IMAGE_TAG:-v2.14.1-beta.1}"
      exit 0
    fi
    cat <<YAML
services:
  ftw:
    environment:
      FTW_IMAGE_TAG: ${FTW_IMAGE_TAG:-v2.14.1-beta.1}
    image: ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-v2.14.1-beta.1}
    volumes:
      - type: bind
        source: ${data}
        target: /app/data
YAML
    ;;
  ps)
    if [ "${2:-}" = -q ] || [ "${3:-}" = -q ]; then
      service=""
      while [ "$#" -gt 0 ]; do
        case "$1" in
          ftw|ftw-updater) service="$1"; shift ;;
          *) shift ;;
        esac
      done
      if [ "$service" = ftw-updater ]; then
        echo updater-id
      else
        echo core-id
      fi
    fi
    ;;
  pull)
    service="${2:-}"
    if [ "$service" = ftw-updater ]; then
      echo "pulled updater"
    elif [ "$service" = ftw ]; then
      echo "pulled core"
    fi
    ;;
  up)
    service=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        ftw|ftw-updater) service="$1"; shift ;;
        *) shift ;;
      esac
    done
    if [ "$service" = ftw-updater ]; then
      set_image ftw-updater "ghcr.io/srcfl/ftw-updater:${target}"
    elif [ "$service" = ftw ]; then
      set_image ftw "ghcr.io/srcfl/ftw:${target}"
    fi
    ;;
  logs)
    exit 0
    ;;
  *)
    exit 1
    ;;
esac
EOF
chmod +x "$TMP/bin/uname" "$TMP/bin/curl" "$TMP/bin/chmod" "$TMP/bin/docker"

export PATH="$TMP/bin:$PATH"
export FAKE_STATE_DIR="$TMP/state"
export FAKE_DATA_DIR="$TMP/site/data"
export FAKE_TARGET_TAG="v3.4.2-beta.1"
export DOCKER_LOG="$TMP/docker.log"
export FAKE_CAPS_JSON='{"protocol":1,"preserve_core_on_readiness_failure":true}'
export FTW_UPDATER_READY_SECS=4
export FTW_READY_TIMEOUT_SECS=5

: >"$DOCKER_LOG"
PATH="$TMP/bin:$PATH" bash "$SCRIPT" --tag v3.4.2-beta.1 --dir "$TMP/site" --backup-dir "$TMP/site/ftw-backups"

grep -q 'FTW_UPDATER_IMAGE_TAG=v3.4.2-beta.1' "$TMP/site/.env"
grep -q 'FTW_IMAGE_TAG=v3.4.2-beta.1' "$TMP/site/.env"
grep -q '# keep me' "$TMP/site/.env"
grep -q 'FTW_API_TOKEN=secret' "$TMP/site/.env"
test -f "$TMP/site/ftw-backups/ftw-full-backup-test.ftwbak"

python3 - "$DOCKER_LOG" <<'PY'
from pathlib import Path
import sys
log = Path(sys.argv[1]).read_text().splitlines()
joined = "\n".join(log)
if "pull ftw-updater" not in joined:
    raise SystemExit("missing updater pull")
if "force-recreate ftw-updater" not in joined:
    raise SystemExit("missing updater recreate")
if not any(line.rstrip().endswith("pull ftw") for line in log):
    raise SystemExit("missing core pull")
if not any(line.rstrip().endswith("force-recreate ftw") for line in log):
    raise SystemExit("missing core recreate")
updater_up = next(i for i, line in enumerate(log) if "force-recreate ftw-updater" in line)
core_pull = next(i for i, line in enumerate(log) if line.rstrip().endswith("pull ftw"))
if updater_up > core_pull:
    raise SystemExit("core pulled before updater was recreated")
PY

# Capabilities failure must not recreate Core.
mkdir -p "$TMP/fail/data" "$TMP/fail/ftw-backups" "$TMP/fail-state"
: >"$TMP/fail/data/state.db"
cp "$TMP/site/docker-compose.yml" "$TMP/fail/docker-compose.yml"
printf 'FTW_IMAGE_TAG=v2.14.1-beta.1\n' >"$TMP/fail/.env"
: >"$TMP/fail.log"
export FAKE_STATE_DIR="$TMP/fail-state"
export FAKE_DATA_DIR="$TMP/fail/data"
export DOCKER_LOG="$TMP/fail.log"
unset FAKE_CAPS_JSON
export FTW_UPDATER_READY_SECS=2
if PATH="$TMP/bin:$PATH" bash "$SCRIPT" --tag v3.4.2-beta.1 --dir "$TMP/fail" --backup-dir "$TMP/fail/ftw-backups" >/dev/null 2>"$TMP/fail.err"; then
  echo "capabilities failure should abort" >&2
  exit 1
fi
grep -q 'did not advertise safe /capabilities' "$TMP/fail.err"
if grep -E 'pull ftw$' "$TMP/fail.log" || grep -E 'force-recreate ftw$' "$TMP/fail.log"; then
  echo "Core was pulled or recreated after a failed updater probe" >&2
  cat "$TMP/fail.log" >&2
  exit 1
fi
grep -q 'force-recreate ftw-updater' "$TMP/fail.log"

echo "upgrade-paired-release script tests passed"
