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
[ "${FAKE_FAIL_READY:-}" != 1 ] || exit 1
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
  pull)
    exit 0
    ;;
  run)
    backup_mount=""
    entrypoint=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --rm)
          shift
          ;;
        --user|--entrypoint)
          if [ "$1" = --entrypoint ]; then
            entrypoint="${2:-}"
          fi
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
    elif [ "$entrypoint" = chown ]; then
      exit 0
    else
      exit 1
    fi
    ;;
  ps)
    if [ -n "${FAKE_PROJECT_NAME:-}" ]; then
      echo generated-container-id
    fi
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
      *'{{.Image}}'*)
        case "$subject" in
          ftw-id) echo 'sha256:old-core' ;;
          ftw-updater-id) echo 'sha256:old-updater' ;;
          ftw-optimizer-id) echo 'sha256:old-optimizer' ;;
          *) exit 1 ;;
        esac
        ;;
      *project.working_dir*)
        echo "${FAKE_INSTALL_DIR:-<no value>}"
        ;;
      *com.docker.compose.project*)
        echo "${FAKE_PROJECT_NAME:-<no value>}"
        ;;
      *Config.Image*)
        case "$subject" in
          ftw-id)
            if [ -f "$state/canonical-ftw" ]; then echo "ghcr.io/srcfl/ftw:$(cat "$state/canonical-ftw")"; else echo 'example.invalid/old-core:latest'; fi
            ;;
          ftw-updater-id)
            if [ -f "$state/canonical-ftw-updater" ]; then echo "ghcr.io/srcfl/ftw-updater:$(cat "$state/canonical-ftw-updater")"; else echo 'example.invalid/old-updater:latest'; fi
            ;;
          ftw-optimizer-id)
            if [ -f "$state/canonical-ftw-optimizer" ]; then echo 'ghcr.io/srcfl/ftw-optimizer:latest'; else echo 'example.invalid/old-optimizer:latest'; fi
            ;;
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
      *State.Health*)
        echo healthy
        ;;
      *)
        echo '<no value>'
        ;;
    esac
    ;;
  compose)
    while [ "${1:-}" = --project-name ] || [ "${1:-}" = -f ]; do
      if [ "${1:-}" = --project-name ]; then
        touch "$state/project-$2"
      fi
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
            if grep -q '^  ftw-optimizer:' docker-compose.yml || \
              { [ -f docker-compose.override.yml ] && grep -q '^  ftw-optimizer:' docker-compose.override.yml; }; then
              echo ftw-optimizer
            fi
            ;;
          --images)
            case "${2:-}" in
              ftw) echo "ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}" ;;
              ftw-updater) echo "ghcr.io/srcfl/ftw-updater:${FTW_UPDATER_IMAGE_TAG:-latest}" ;;
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
        case "$service" in
          ftw) printf '%s\n' "${FTW_IMAGE_TAG:-latest}" >"$state/canonical-$service" ;;
          ftw-updater) printf '%s\n' "${FTW_UPDATER_IMAGE_TAG:-latest}" >"$state/canonical-$service" ;;
          ftw-optimizer) printf '%s\n' "${FTW_OPTIMIZER_IMAGE_TAG:-latest}" >"$state/canonical-$service" ;;
        esac
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
  image)
    [ "${1:-}" = tag ] || exit 1
    printf '%s %s\n' "${2:-}" "${3:-}" >>"$state/image-tags"
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
touch "$TMP/migrate/data/state.db"

PATH="$TMP/migrate/bin:$PATH" \
FAKE_STATE_DIR="$TMP/migrate/state" \
FAKE_DATA_DIR="$TMP/migrate/data" \
bash "$ROOT/scripts/migrate-legacy-compose.sh" --version v1.4.0 --dir "$TMP/migrate"

grep -q '^  ftw-optimizer:' "$TMP/migrate/docker-compose.override.yml"
test -f "$TMP/migrate/state/ftw"
test -f "$TMP/migrate/state/ftw-updater"
test -f "$TMP/migrate/state/ftw-optimizer"
test -f "$TMP/migrate"/.ftw-migration-backup-*/previous-images.tsv

# Generated container names still carry Compose labels. The migration must
# reuse their explicit project name instead of creating a parallel default.
mkdir -p "$TMP/project/data" "$TMP/project/state"
touch "$TMP/project/data/state.db"
cp "$TMP/migrate/docker-compose.yml" "$TMP/project/docker-compose.yml"
PATH="$TMP/migrate/bin:$PATH" \
FAKE_STATE_DIR="$TMP/project/state" \
FAKE_DATA_DIR="$TMP/project/data" \
FAKE_PROJECT_NAME=custom-energy \
FAKE_INSTALL_DIR="$TMP/project" \
bash "$ROOT/scripts/migrate-legacy-compose.sh" --version v1.4.0 --dir "$TMP/project"
test -e "$TMP/project/state/project-custom-energy"

if [ -n "$REAL_DOCKER" ]; then
  "$REAL_DOCKER" compose \
    -f "$TMP/migrate/docker-compose.yml" \
    -f "$TMP/migrate/docker-compose.override.yml" \
    config --quiet
fi

mkdir -p "$TMP/rollback/data" "$TMP/rollback/state"
touch "$TMP/rollback/data/state.db"
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
  bash "$ROOT/scripts/migrate-legacy-compose.sh" --version v1.4.0 --dir "$TMP/rollback" \
  >/dev/null 2>&1; then
  echo "expected a failed core recreate to roll the modular migration back" >&2
  exit 1
fi
test ! -e "$TMP/rollback/docker-compose.override.yml"
grep -q 'example.invalid/old-core:latest' "$TMP/rollback/docker-compose.yml"
grep -q 'example.invalid/old-updater:latest' "$TMP/rollback/docker-compose.yml"
test ! -e "$TMP/rollback/state/ftw-optimizer"

# An already-modular legacy layout must update the optimizer too, and a later
# failure must recreate its previous image alongside core + updater.
mkdir -p "$TMP/rollback-existing/data" "$TMP/rollback-existing/state"
touch "$TMP/rollback-existing/data/state.db"
cat >"$TMP/rollback-existing/docker-compose.yml" <<'YAML'
services:
  ftw:
    image: example.invalid/old-core:latest
    volumes:
      - ./data:/app/data
  ftw-updater:
    image: example.invalid/old-updater:latest
  ftw-optimizer:
    image: example.invalid/old-optimizer:latest
YAML
touch \
  "$TMP/rollback-existing/state/ftw" \
  "$TMP/rollback-existing/state/ftw-updater" \
  "$TMP/rollback-existing/state/ftw-optimizer"

if PATH="$TMP/migrate/bin:$PATH" \
  FAKE_STATE_DIR="$TMP/rollback-existing/state" \
  FAKE_DATA_DIR="$TMP/rollback-existing/data" \
  FAKE_FAIL_SERVICE=ftw \
  bash "$ROOT/scripts/migrate-legacy-compose.sh" --version v1.4.0 --dir "$TMP/rollback-existing" \
  >/dev/null 2>&1; then
  echo "expected an existing modular migration failure to roll back" >&2
  exit 1
fi
grep -q 'example.invalid/old-optimizer:latest' "$TMP/rollback-existing/docker-compose.yml"
test -e "$TMP/rollback-existing/state/ftw-optimizer"
grep -q '^sha256:old-core example.invalid/old-core:latest$' "$TMP/rollback-existing/state/image-tags"
grep -q '^sha256:old-updater example.invalid/old-updater:latest$' "$TMP/rollback-existing/state/image-tags"
grep -q '^sha256:old-optimizer example.invalid/old-optimizer:latest$' "$TMP/rollback-existing/state/image-tags"

# A failure after both candidates were recreated must still restore both old
# control-plane image IDs and never report migration success.
mkdir -p "$TMP/readiness-rollback/data" "$TMP/readiness-rollback/state"
touch "$TMP/readiness-rollback/data/state.db"
touch "$TMP/readiness-rollback/state/ftw" "$TMP/readiness-rollback/state/ftw-updater"
cat >"$TMP/readiness-rollback/docker-compose.yml" <<'YAML'
services:
  ftw:
    image: example.invalid/old-core:latest
    volumes:
      - ./data:/app/data
  ftw-updater:
    image: example.invalid/old-updater:latest
YAML
if PATH="$TMP/migrate/bin:$PATH" \
  FAKE_STATE_DIR="$TMP/readiness-rollback/state" \
  FAKE_DATA_DIR="$TMP/readiness-rollback/data" \
  FAKE_FAIL_READY=1 \
  FTW_MIGRATION_HEALTH_TIMEOUT_S=1 \
  bash "$ROOT/scripts/migrate-legacy-compose.sh" --version v1.4.0 --dir "$TMP/readiness-rollback" \
  >/dev/null 2>&1; then
  echo "expected readiness failure to roll back the paired migration" >&2
  exit 1
fi
grep -q 'example.invalid/old-core:latest' "$TMP/readiness-rollback/docker-compose.yml"
grep -q 'example.invalid/old-updater:latest' "$TMP/readiness-rollback/docker-compose.yml"
grep -q '^sha256:old-core example.invalid/old-core:latest$' "$TMP/readiness-rollback/state/image-tags"
grep -q '^sha256:old-updater example.invalid/old-updater:latest$' "$TMP/readiness-rollback/state/image-tags"

# Optimizer is an independent, optional phase. A failed optimizer candidate
# must leave the newly healthy Core + updater online and must not fail the
# migration or activate a driver.
mkdir -p "$TMP/optimizer-failure/data" "$TMP/optimizer-failure/state"
touch "$TMP/optimizer-failure/data/state.db"
cat >"$TMP/optimizer-failure/docker-compose.yml" <<'YAML'
services:
  ftw:
    image: example.invalid/old-core:latest
    volumes:
      - ./data:/app/data
  ftw-updater:
    image: example.invalid/old-updater:latest
YAML
PATH="$TMP/migrate/bin:$PATH" \
FAKE_STATE_DIR="$TMP/optimizer-failure/state" \
FAKE_DATA_DIR="$TMP/optimizer-failure/data" \
FAKE_FAIL_SERVICE=ftw-optimizer \
bash "$ROOT/scripts/migrate-legacy-compose.sh" --version v1.4.0 --dir "$TMP/optimizer-failure" \
  >/dev/null
test -e "$TMP/optimizer-failure/state/ftw"
test -e "$TMP/optimizer-failure/state/ftw-updater"
test ! -e "$TMP/optimizer-failure/state/ftw-optimizer"
grep -q 'ghcr.io/srcfl/ftw:' "$TMP/optimizer-failure/docker-compose.yml"

echo "modular Compose migration tests passed"
