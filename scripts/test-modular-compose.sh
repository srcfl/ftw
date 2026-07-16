#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
REAL_DOCKER="$(command -v docker || true)"

mkdir -p "$TMP/bin" "$TMP/fresh" "$TMP/custom"
DOCKER_LOG="$TMP/docker.log"
export DOCKER_LOG

cat >"$TMP/bin/docker" <<'FAKE_DOCKER'
#!/usr/bin/env bash
set -euo pipefail

[ "${1:-}" = compose ] || exit 1
shift
files=()
while [ "${1:-}" = -f ]; do
  files+=("$2")
  shift 2
done

case "${1:-}" in
  config)
    if [ "${2:-}" = --services ]; then
      echo ftw
      for file in "${files[@]}"; do
        if grep -q '^  ftw-optimizer:' "$file"; then
          echo ftw-optimizer
        fi
      done
    fi
    ;;
  up)
    printf '%s\n' "$*" >>"$DOCKER_LOG"
    ;;
  *)
    exit 1
    ;;
esac
FAKE_DOCKER
chmod +x "$TMP/bin/docker"

write_base() {
  path="$1"
  cat >"$path" <<'YAML'
services:
  ftw:
    image: ghcr.io/srcfl/ftw:latest
    volumes:
      - ./data:/app/data
YAML
}

write_base "$TMP/fresh/docker-compose.yml"
PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/enable-modular-stack.sh" \
  "$TMP/fresh/docker-compose.yml"

override="$TMP/fresh/docker-compose.override.yml"
test -f "$override"
grep -q '^  ftw-optimizer:' "$override"
grep -q 'optimizer-ipc:/run/ftw-optimizer' "$override"
grep -q '^up -d ftw-optimizer ftw$' "$DOCKER_LOG"

cp "$override" "$TMP/override.before"
PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/enable-modular-stack.sh" \
  "$TMP/fresh/docker-compose.yml"
cmp "$TMP/override.before" "$override"

write_base "$TMP/custom/docker-compose.yml"
cat >"$TMP/custom/docker-compose.override.yml" <<'YAML'
services:
  ftw:
    environment:
      OPERATOR_SETTING: preserved
YAML

if PATH="$TMP/bin:$PATH" bash "$ROOT/scripts/enable-modular-stack.sh" \
  "$TMP/custom/docker-compose.yml" >/dev/null 2>&1; then
  echo "expected a custom override without ftw-optimizer to fail closed" >&2
  exit 1
fi
grep -q 'OPERATOR_SETTING: preserved' "$TMP/custom/docker-compose.override.yml"

mkdir -p "$TMP/migrate/bin" "$TMP/migrate/data" "$TMP/migrate/state"
cat >"$TMP/migrate/docker-compose.yml" <<'YAML'
services:
  ftw:
    image: example.invalid/old-core:latest
    volumes:
      - ./data:/app/data
  ftw-updater:
    image: example.invalid/old-updater:latest
YAML

cat >"$TMP/migrate/bin/uname" <<'FAKE_UNAME'
#!/usr/bin/env bash
echo Linux
FAKE_UNAME

cat >"$TMP/migrate/bin/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
exit 0
FAKE_CURL

cat >"$TMP/migrate/bin/chmod" <<'FAKE_CHMOD'
#!/usr/bin/env bash
# The production migration is Linux-only and uses GNU chmod --reference.
# The test also runs from macOS, where preserving fixture modes is irrelevant.
exit 0
FAKE_CHMOD

cat >"$TMP/migrate/bin/docker" <<'FAKE_MIGRATION_DOCKER'
#!/usr/bin/env bash
set -euo pipefail

state="${FAKE_STATE_DIR:?}"
data="${FAKE_DATA_DIR:?}"
command="${1:-}"
shift || true

case "$command" in
  info)
    exit 0
    ;;
  container)
    [ "${1:-}" = inspect ] || exit 1
    [ -f "$state/${2:-}" ]
    ;;
  inspect)
    subject="${1:-}"
    shift || true
    format="$*"
    case "$format" in
      *Config.Image*)
        case "$subject" in
          ftw-id) echo 'ghcr.io/srcfl/ftw:latest' ;;
          ftw-updater-id) echo 'ghcr.io/srcfl/ftw-updater:latest' ;;
          ftw-optimizer-id) echo 'ghcr.io/srcfl/ftw-optimizer:latest' ;;
          *) exit 1 ;;
        esac
        ;;
      *Mounts*)
        [ "$subject" = ftw-id ] || exit 1
        echo "$data"
        ;;
      *State.Running*)
        echo true
        ;;
      *)
        echo '<no value>'
        ;;
    esac
    ;;
  compose)
    while [ "${1:-}" = --project-name ] || [ "${1:-}" = -f ]; do
      shift 2
    done
    subcommand="${1:-}"
    shift || true
    case "$subcommand" in
      version)
        exit 0
        ;;
      config)
        case "${1:-}" in
          --services)
            echo ftw
            echo ftw-updater
            if [ -f docker-compose.override.yml ] && \
              grep -q '^  ftw-optimizer:' docker-compose.override.yml; then
              echo ftw-optimizer
            fi
            ;;
          --images)
            case "${2:-}" in
              ftw) echo 'ghcr.io/srcfl/ftw:latest' ;;
              ftw-updater) echo 'ghcr.io/srcfl/ftw-updater:latest' ;;
              ftw-optimizer) echo 'ghcr.io/srcfl/ftw-optimizer:latest' ;;
              *) exit 1 ;;
            esac
            ;;
          ftw)
            printf 'volumes:\n  - type: bind\n    source: %s\n    target: /app/data\n' "$data"
            ;;
        esac
        ;;
      pull)
        exit 0
        ;;
      up)
        service="${*: -1}"
        if [ -n "${FAKE_FAIL_SERVICE:-}" ] && \
          [ "$service" = "$FAKE_FAIL_SERVICE" ] && \
          [ ! -f "$state/failure-consumed" ]; then
          touch "$state/failure-consumed"
          exit 1
        fi
        touch "$state/$service" "$state/$service-id"
        ;;
      ps)
        service="${*: -1}"
        if [ -f "$state/$service" ]; then
          echo "$service-id"
        fi
        ;;
      rm)
        service="${*: -1}"
        rm -f "$state/$service" "$state/$service-id"
        ;;
      *)
        exit 1
        ;;
    esac
    ;;
  *)
    exit 1
    ;;
esac
FAKE_MIGRATION_DOCKER
chmod +x \
  "$TMP/migrate/bin/uname" \
  "$TMP/migrate/bin/curl" \
  "$TMP/migrate/bin/chmod" \
  "$TMP/migrate/bin/docker"

PATH="$TMP/migrate/bin:$PATH" \
FAKE_STATE_DIR="$TMP/migrate/state" \
FAKE_DATA_DIR="$TMP/migrate/data" \
bash "$ROOT/scripts/migrate-legacy-compose.sh" --dir "$TMP/migrate"

grep -q '^  ftw-optimizer:' "$TMP/migrate/docker-compose.override.yml"
test -f "$TMP/migrate/state/ftw"
test -f "$TMP/migrate/state/ftw-updater"
test -f "$TMP/migrate/state/ftw-optimizer"

if [ -n "$REAL_DOCKER" ]; then
  "$REAL_DOCKER" compose \
    -f "$TMP/migrate/docker-compose.yml" \
    -f "$TMP/migrate/docker-compose.override.yml" \
    config --quiet
fi

mkdir -p "$TMP/rollback/data" "$TMP/rollback/state"
cat >"$TMP/rollback/docker-compose.yml" <<'YAML'
services:
  ftw:
    image: example.invalid/old-core:latest
    volumes:
      - ./data:/app/data
  ftw-updater:
    image: example.invalid/old-updater:latest
YAML

if PATH="$TMP/migrate/bin:$PATH" \
  FAKE_STATE_DIR="$TMP/rollback/state" \
  FAKE_DATA_DIR="$TMP/rollback/data" \
  FAKE_FAIL_SERVICE=ftw \
  bash "$ROOT/scripts/migrate-legacy-compose.sh" --dir "$TMP/rollback" \
  >/dev/null 2>&1; then
  echo "expected a failed core recreate to roll the modular migration back" >&2
  exit 1
fi
test ! -e "$TMP/rollback/docker-compose.override.yml"
grep -q 'example.invalid/old-core:latest' "$TMP/rollback/docker-compose.yml"
grep -q 'example.invalid/old-updater:latest' "$TMP/rollback/docker-compose.yml"
test ! -e "$TMP/rollback/state/ftw-optimizer"

echo "modular Compose migration tests passed"
