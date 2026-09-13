#!/usr/bin/env bash

# Move an existing canonical Compose install to one published Core + updater
# tag. The orange Update button recreates Core first and only replaces the
# sidecar after Core is healthy; the first DuckDB hop needs the new updater
# already running. This script pins and recreates ftw-updater, checks
# GET /capabilities, then recreates Core on the same immutable tag.

set -Eeuo pipefail

umask 077

log() {
  printf '[FTW upgrade] %s\n' "$*"
}

die() {
  printf '[FTW upgrade] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: upgrade-paired-release.sh --tag vX.Y.Z[-beta.N] [--dir PATH] [--backup-dir PATH]

Installs one published Core + updater pair. The updater is replaced first,
while the current Core keeps running, then Core is moved to the same tag.

Do not pass :latest, :beta, or v3.2.0-beta.1. Pick a published GitHub
Release tag whose updater includes the DuckDB-safe timeout (v3.2.1-beta.1
or newer).

Without --dir, the script uses the current directory when it contains
docker-compose.yml, then tries ~/ftw and ~/forty-two-watts.

The verified .ftwbak is written outside the live data directory. Prefer a
USB disk or network share for --backup-dir. The default is
<installation>/ftw-backups.
EOF
}

requested_dir="${FTW_DIR:-}"
requested_full_backup_dir="${FTW_BACKUP_DIR:-}"
target_tag=""
ready_timeout_secs="${FTW_READY_TIMEOUT_SECS:-21600}"
updater_ready_secs="${FTW_UPDATER_READY_SECS:-60}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --tag)
      [ "$#" -ge 2 ] || die "--tag requires a published release tag"
      target_tag="$2"
      shift 2
      ;;
    --dir)
      [ "$#" -ge 2 ] || die "--dir requires a path"
      requested_dir="$2"
      shift 2
      ;;
    --backup-dir)
      [ "$#" -ge 2 ] || die "--backup-dir requires a path"
      requested_full_backup_dir="$2"
      shift 2
      ;;
    --ready-timeout)
      [ "$#" -ge 2 ] || die "--ready-timeout requires seconds"
      ready_timeout_secs="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

validate_tag() {
  local tag="$1"
  case "$tag" in
    ""|latest|beta|edge|stable|":latest"|":beta"|*:latest|*:beta)
      die "refusing moving image alias $tag; pass a published vX.Y.Z or vX.Y.Z-beta.N tag"
      ;;
    v3.2.0-beta.1)
      die "v3.2.0-beta.1 does not contain the DuckDB updater fix; pick v3.2.1-beta.1 or newer"
      ;;
  esac
  case "$tag" in
    v[0-9]*.[0-9]*.[0-9]*-beta.[0-9]*) ;;
    v[0-9]*.[0-9]*.[0-9]*) ;;
    *)
      die "tag $tag is not vX.Y.Z or vX.Y.Z-beta.N"
      ;;
  esac
}

[ -n "$target_tag" ] || die "missing --tag (example: --tag v3.4.2-beta.1)"
validate_tag "$target_tag"
case "$ready_timeout_secs" in
  ''|*[!0-9]*) die "--ready-timeout must be seconds" ;;
esac
case "$updater_ready_secs" in
  ''|*[!0-9]*) die "FTW_UPDATER_READY_SECS must be seconds" ;;
esac

[ "$(uname -s)" = Linux ] || die "this upgrade currently supports Linux Docker hosts only"
command -v docker >/dev/null 2>&1 || die "docker is not installed"
command -v curl >/dev/null 2>&1 || die "curl is not installed"
docker compose version >/dev/null 2>&1 || die "docker compose is not available"
docker info >/dev/null 2>&1 || \
  die "cannot access the Docker daemon; log out and in after joining the docker group, then retry"

if [ -n "$requested_dir" ]; then
  install_dir="$requested_dir"
elif [ -f "$PWD/docker-compose.yml" ]; then
  install_dir="$PWD"
elif [ -f "$HOME/ftw/docker-compose.yml" ] && [ -f "$HOME/forty-two-watts/docker-compose.yml" ]; then
  die "both ~/ftw and ~/forty-two-watts contain installations; rerun with --dir PATH"
elif [ -f "$HOME/ftw/docker-compose.yml" ]; then
  install_dir="$HOME/ftw"
elif [ -f "$HOME/forty-two-watts/docker-compose.yml" ]; then
  install_dir="$HOME/forty-two-watts"
else
  die "could not find docker-compose.yml; rerun with --dir PATH"
fi

[ -d "$install_dir" ] || die "installation directory does not exist: $install_dir"
install_dir="$(cd "$install_dir" && pwd -P)"
compose_file="$install_dir/docker-compose.yml"
[ -f "$compose_file" ] || die "missing $compose_file"
cd "$install_dir"

compose_project=""
remember_compose_project() {
  candidate_project="$1"
  case "$candidate_project" in
    ""|"<no value>") return ;;
  esac
  if [ -n "$compose_project" ] && [ "$candidate_project" != "$compose_project" ]; then
    die "existing FTW containers belong to different Compose projects"
  fi
  compose_project="$candidate_project"
}
for known_container in ftw forty-two-watts ftw-updater; do
  if ! docker container inspect "$known_container" >/dev/null 2>&1; then
    continue
  fi
  candidate_project="$(docker inspect "$known_container" --format '{{index .Config.Labels "com.docker.compose.project"}}')"
  remember_compose_project "$candidate_project"
done
for known_service in ftw forty-two-watts ftw-updater; do
  while IFS= read -r container_id; do
    [ -n "$container_id" ] || continue
    candidate_workdir="$(docker inspect "$container_id" --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}')"
    if [ -d "$candidate_workdir" ]; then
      candidate_workdir="$(cd "$candidate_workdir" && pwd -P)"
    fi
    [ "$candidate_workdir" = "$install_dir" ] || continue
    candidate_project="$(docker inspect "$container_id" --format '{{index .Config.Labels "com.docker.compose.project"}}')"
    remember_compose_project "$candidate_project"
  done < <(docker ps -aq --filter "label=com.docker.compose.service=$known_service")
done

compose_command=(docker compose)
if [ -n "$compose_project" ]; then
  compose_command+=(--project-name "$compose_project")
  log "preserving Compose project: $compose_project"
fi
compose() {
  "${compose_command[@]}" "$@"
}

image_tag() {
  local image="$1"
  case "$image" in
    *:*) printf '%s\n' "${image##*:}" ;;
    *) printf '%s\n' "$image" ;;
  esac
}

# Rewrite KEY=value assignments in .env. Comments, blanks and unrelated
# keys stay as they were. Compose reads the last assignment, so older
# duplicates of a managed key are dropped.
pin_env_keys() {
  local env_file="$1"
  shift
  local tmp out key value line trimmed assign_key
  tmp="${env_file}.ftw-upgrade-tmp"
  out="${tmp}.out"
  if [ -e "$env_file" ]; then
    cp -p "$env_file" "$tmp"
  else
    rm -f "$tmp"
    : >"$tmp"
    chmod 600 "$tmp"
  fi

  declare -A wanted last
  local -a keys=()
  for pair in "$@"; do
    key="${pair%%=*}"
    value="${pair#*=}"
    [ -n "$key" ] || die "internal: empty env key"
    if [ -z "${wanted[$key]+x}" ]; then
      keys+=("$key")
    fi
    wanted["$key"]="$value"
  done

  local i=0
  while IFS= read -r line || [ -n "$line" ]; do
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in
      ""|\#*) ;;
      export\ *)
        trimmed="${trimmed#export }"
        ;;
    esac
    case "$trimmed" in
      *=*)
        assign_key="${trimmed%%=*}"
        assign_key="${assign_key%"${assign_key##*[![:space:]]}"}"
        if [ -n "${wanted[$assign_key]+x}" ]; then
          last["$assign_key"]="$i"
        fi
        ;;
    esac
    i=$((i + 1))
  done <"$tmp"

  : >"$out"
  chmod --reference="$tmp" "$out"
  i=0
  while IFS= read -r line || [ -n "$line" ]; do
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in
      ""|\#*)
        printf '%s\n' "$line" >>"$out"
        i=$((i + 1))
        continue
        ;;
      export\ *)
        trimmed="${trimmed#export }"
        ;;
    esac
    case "$trimmed" in
      *=*)
        assign_key="${trimmed%%=*}"
        assign_key="${assign_key%"${assign_key##*[![:space:]]}"}"
        if [ -n "${wanted[$assign_key]+x}" ]; then
          if [ "${last[$assign_key]}" -eq "$i" ]; then
            printf '%s=%s\n' "$assign_key" "${wanted[$assign_key]}" >>"$out"
            unset "wanted[$assign_key]"
          fi
          i=$((i + 1))
          continue
        fi
        ;;
    esac
    printf '%s\n' "$line" >>"$out"
    i=$((i + 1))
  done <"$tmp"

  for key in "${keys[@]}"; do
    if [ -n "${wanted[$key]+x}" ]; then
      printf '%s=%s\n' "$key" "${wanted[$key]}" >>"$out"
    fi
  done
  mv "$out" "$env_file"
  rm -f "$tmp"
}

running_image() {
  local service="$1"
  local id
  id="$(compose ps -q --status running "$service" | tail -n 1)"
  [ -n "$id" ] || return 1
  docker inspect "$id" --format '{{.Config.Image}}'
}

ipc_volume_name() {
  local service="$1"
  local id
  id="$(compose ps -q --status running "$service" | tail -n 1)"
  [ -n "$id" ] || return 1
  docker inspect "$id" --format '{{range .Mounts}}{{if eq .Destination "/run/ftw-update"}}{{.Name}}{{end}}{{end}}'
}

probe_updater_capabilities() {
  local volume="$1"
  local payload
  payload="$(docker run --rm --network none \
    -v "$volume:/run/ftw-update:ro" \
    --entrypoint wget \
    "ghcr.io/srcfl/ftw:${target_tag}" \
    -qO- --timeout=5 --unix-socket=/run/ftw-update/sock http://localhost/capabilities 2>/dev/null || true)"
  case "$payload" in
    *'"protocol":1'*|*'\"protocol\": 1'*|*'"protocol": 1'*)
      ;;
    *)
      return 1
      ;;
  esac
  case "$payload" in
    *'"preserve_core_on_readiness_failure":true'*|*'"preserve_core_on_readiness_failure": true'*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

services="$(compose config --services)"
has_ftw=false
has_legacy=false
if printf '%s\n' "$services" | grep -qx 'ftw'; then
  has_ftw=true
fi
if printf '%s\n' "$services" | grep -qx 'forty-two-watts'; then
  has_legacy=true
fi
if [ "$has_ftw" = true ] && [ "$has_legacy" = true ]; then
  die "both ftw and forty-two-watts services exist; refusing to guess"
elif [ "$has_ftw" = true ]; then
  main_service="ftw"
elif [ "$has_legacy" = true ]; then
  main_service="forty-two-watts"
else
  die "no ftw or forty-two-watts main service found"
fi
printf '%s\n' "$services" | grep -qx 'ftw-updater' || die "ftw-updater is missing; this layout needs manual review"

config_check="$(mktemp)"
identity_probe="ftw-image-tag-probe"
FTW_IMAGE_TAG="$identity_probe" "${compose_command[@]}" config "$main_service" >"$config_check"
if ! grep -Eq "^[[:space:]]+FTW_IMAGE_TAG:[[:space:]]*[\"']?${identity_probe}[\"']?[[:space:]]*$" "$config_check"; then
  die "Compose does not pass FTW_IMAGE_TAG into $main_service; add that mapping before using this script"
fi
data_mount="$(awk '
  /^[[:space:]]*-[[:space:]]+type:/ {
    type = $0
    sub(/^.*type:[[:space:]]*/, "", type)
    source = ""
  }
  /^[[:space:]]+source:/ {
    source = $0
    sub(/^[[:space:]]*source:[[:space:]]*/, "", source)
    gsub(/^"|"$/, "", source)
  }
  /^[[:space:]]+target:[[:space:]]*\/app\/data[[:space:]]*$/ {
    print type "|" source
    exit
  }
' "$config_check")"
[ -n "$data_mount" ] || die "main service does not expose a persistent /app/data mount"
data_mount_type="${data_mount%%|*}"
data_mount_source="${data_mount#*|}"
[ "$data_mount_type" = bind ] || die "the /app/data mount must be a host bind"
[ -d "$data_mount_source" ] || die "the /app/data source does not exist: $data_mount_source"
expected_data_source="$(cd "$data_mount_source" && pwd -P)"
rm -f "$config_check"

effective_images="$(FTW_IMAGE_TAG="$target_tag" FTW_UPDATER_IMAGE_TAG="$target_tag" "${compose_command[@]}" config --images)"
printf '%s\n' "$effective_images" | grep -qx "ghcr.io/srcfl/ftw-updater:${target_tag}" || \
  die "Compose does not pin ftw-updater through FTW_UPDATER_IMAGE_TAG; use docs/upgrade-from-legacy.md for an older layout"
printf '%s\n' "$effective_images" | grep -qx "ghcr.io/srcfl/ftw:${target_tag}" || \
  die "Compose does not pin $main_service through FTW_IMAGE_TAG; use docs/upgrade-from-legacy.md for an older layout"

state_path="$expected_data_source/state.db"
[ -f "$state_path" ] || die "missing $state_path; this script does not follow a custom state.path"

if [ -n "$requested_full_backup_dir" ]; then
  full_backup_dir="$requested_full_backup_dir"
else
  full_backup_dir="$install_dir/ftw-backups"
fi
mkdir -p "$full_backup_dir"
full_backup_dir="$(cd "$full_backup_dir" && pwd -P)"
case "$full_backup_dir/" in
  "$expected_data_source/"*) die "--backup-dir must be outside the live /app/data directory" ;;
esac

lock_dir="$install_dir/.ftw-upgrade.lock"
if ! mkdir "$lock_dir" 2>/dev/null; then
  die "another upgrade may be active ($lock_dir exists)"
fi

success=false
core_recreated=false
core_may_have_opened_data=false
previous_main_image=""
previous_updater_image=""
env_file="$install_dir/.env"
env_backup=""
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
health_url="${FTW_HEALTH_URL:-http://127.0.0.1:8080/api/health}"
ready_url="${FTW_READY_URL:-${health_url%/api/health}/api/status}"

restore_previous_core_image() {
  [ -n "$previous_main_image" ] || return 1
  pin_env_keys "$env_file" "FTW_UPDATER_IMAGE_TAG=${target_tag}" \
    "FTW_IMAGE_TAG=$(image_tag "$previous_main_image")"
  FTW_IMAGE_TAG="$(image_tag "$previous_main_image")" \
    FTW_UPDATER_IMAGE_TAG="$target_tag" \
    "${compose_command[@]}" up -d --no-deps --force-recreate "$main_service"
}

restore_after_failure() {
  status=$?
  if [ "$success" = true ] || [ "$status" -eq 0 ]; then
    rmdir "$lock_dir" 2>/dev/null || true
    return
  fi
  printf '[FTW upgrade] Upgrade failed.\n' >&2
  if [ "$core_may_have_opened_data" = true ]; then
    printf '[FTW upgrade] Core may already have opened data. Do not swap only the image. Restore the verified .ftwbak with its matching Core if this box is unhealthy.\n' >&2
  elif [ "$core_recreated" = true ]; then
    if restore_previous_core_image >/dev/null 2>&1; then
      printf '[FTW upgrade] Restored previous Core. The new updater was left in place.\n' >&2
    else
      printf '[FTW upgrade] Could not restore previous Core automatically. Use the verified .ftwbak.\n' >&2
    fi
  elif [ -n "$env_backup" ] && [ -f "$env_backup" ]; then
    cp -p "$env_backup" "$env_file" 2>/dev/null || true
    if [ -n "$previous_updater_image" ]; then
      FTW_UPDATER_IMAGE_TAG="$(image_tag "$previous_updater_image")" \
        "${compose_command[@]}" up -d --no-deps --force-recreate ftw-updater >/dev/null 2>&1 || true
    fi
    printf '[FTW upgrade] Previous updater pin was restored. Core was not moved.\n' >&2
  fi
  rmdir "$lock_dir" 2>/dev/null || true
  return "$status"
}
trap restore_after_failure EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

previous_main_image="$(running_image "$main_service" || true)"
previous_updater_image="$(running_image ftw-updater || true)"
[ -n "$previous_updater_image" ] || die "ftw-updater is not running"
[ -n "$previous_main_image" ] || die "$main_service is not running"
current_core_tag="$(image_tag "$previous_main_image")"
log "running Core: $previous_main_image"
log "running updater: $previous_updater_image"
log "target pair: $target_tag"

if [ "$(image_tag "$previous_main_image")" = "$target_tag" ] && \
  [ "$(image_tag "$previous_updater_image")" = "$target_tag" ]; then
  ipc="$(ipc_volume_name ftw-updater || true)"
  if [ -n "$ipc" ] && probe_updater_capabilities "$ipc"; then
    if curl -fsS --max-time 3 "$health_url" >/dev/null 2>&1 && \
      curl -fsS --max-time 3 "$ready_url" >/dev/null 2>&1; then
      success=true
      rmdir "$lock_dir"
      trap - EXIT INT TERM
      log "already running $target_tag with a capable updater"
      exit 0
    fi
  fi
fi

if [ -e "$env_file" ]; then
  env_backup="$install_dir/.env.ftw-upgrade-backup-$timestamp"
  cp -p "$env_file" "$env_backup"
  log "copied .env to $env_backup"
fi

log "phase 1/3: verified full backup (running Core is unchanged)"
log "pulling backup helper ghcr.io/srcfl/ftw:${target_tag}"
docker pull "ghcr.io/srcfl/ftw:${target_tag}" >/dev/null
backup_json="$(docker run --rm --user 0:0 \
  -v "$expected_data_source:/app/data:ro" \
  -v "$full_backup_dir:/backup" \
  --entrypoint /app/ftw-backup \
  "ghcr.io/srcfl/ftw:${target_tag}" \
  create -state /app/data/state.db -data /app/data -output /backup -core-version "$current_core_tag")"
full_backup_id="$(printf '%s\n' "$backup_json" | sed -n 's/^[[:space:]]*"id": "\([^"]*\.ftwbak\)",*$/\1/p')"
case "$full_backup_id" in
  ""|*/*|*..*) die "backup helper returned an invalid archive id" ;;
esac
full_backup_archive="$full_backup_dir/$full_backup_id"
[ -f "$full_backup_archive" ] || die "backup helper did not publish $full_backup_archive"
docker run --rm --user 0:0 \
  -v "$full_backup_dir:/backup" \
  --entrypoint /app/ftw-backup \
  "ghcr.io/srcfl/ftw:${target_tag}" verify -archive "/backup/$full_backup_id" >/dev/null
backup_owner_uid="${SUDO_UID:-$(id -u)}"
backup_owner_gid="${SUDO_GID:-$(id -g)}"
docker run --rm --user 0:0 \
  -v "$full_backup_dir:/backup" \
  --entrypoint chown \
  "ghcr.io/srcfl/ftw:${target_tag}" "$backup_owner_uid:$backup_owner_gid" "/backup/$full_backup_id"
log "verified full backup: $full_backup_archive"
case "$full_backup_dir/" in
  "$install_dir/"*)
    log "WARNING: backup is still on the installation disk. Copy $full_backup_archive off this machine before continuing if the SD card can fail."
    ;;
esac

log "phase 2/3: updater $target_tag first (Core stays on $current_core_tag)"
pin_env_keys "$env_file" "FTW_UPDATER_IMAGE_TAG=${target_tag}"
log "pulling ftw-updater:${target_tag}"
FTW_UPDATER_IMAGE_TAG="$target_tag" "${compose_command[@]}" pull ftw-updater
log "recreating ftw-updater only"
FTW_UPDATER_IMAGE_TAG="$target_tag" "${compose_command[@]}" up -d --no-deps --force-recreate ftw-updater
updater_ready=false
caps_deadline=$((SECONDS + updater_ready_secs))
while [ "$SECONDS" -lt "$caps_deadline" ]; do
  running_updater="$(running_image ftw-updater || true)"
  if [ "$running_updater" = "ghcr.io/srcfl/ftw-updater:${target_tag}" ]; then
    ipc="$(ipc_volume_name ftw-updater || true)"
    if [ -n "$ipc" ] && probe_updater_capabilities "$ipc"; then
      updater_ready=true
      break
    fi
  fi
  sleep 2
done
[ "$updater_ready" = true ] || die "ftw-updater did not advertise safe /capabilities on $target_tag; Core was not moved"
log "updater $target_tag is running and reports preserve_core_on_readiness_failure"

log "phase 3/3: Core $target_tag"
pin_env_keys "$env_file" "FTW_UPDATER_IMAGE_TAG=${target_tag}" "FTW_IMAGE_TAG=${target_tag}"
log "pulling ${main_service}:${target_tag}"
FTW_IMAGE_TAG="$target_tag" FTW_UPDATER_IMAGE_TAG="$target_tag" \
  "${compose_command[@]}" pull "$main_service"
log "recreating $main_service only"
core_recreated=true
FTW_IMAGE_TAG="$target_tag" FTW_UPDATER_IMAGE_TAG="$target_tag" \
  "${compose_command[@]}" up -d --no-deps --force-recreate "$main_service"

early_deadline=$((SECONDS + 120))
ready=false
next_note=$((SECONDS + 60))
ready_deadline=$((SECONDS + ready_timeout_secs))
while [ "$SECONDS" -lt "$ready_deadline" ]; do
  if curl -fsS --max-time 3 "$health_url" >/dev/null 2>&1 && \
    curl -fsS --max-time 3 "$ready_url" >/dev/null 2>&1; then
    ready=true
    break
  fi
  if [ "$SECONDS" -lt "$early_deadline" ]; then
    if compose logs --tail 80 "$main_service" 2>/dev/null | grep -q 'update ftw-updater first'; then
      die "new Core refused to open data (update ftw-updater first)"
    fi
  else
    core_may_have_opened_data=true
  fi
  if [ "$SECONDS" -ge "$next_note" ]; then
    log "still waiting for $ready_url (DuckDB import can take hours on a Pi)"
    next_note=$((SECONDS + 60))
  fi
  sleep 5
done

if [ "$ready" != true ]; then
  log "WARNING: $ready_url was not ready within ${ready_timeout_secs}s. The new Core and data were left in place. Do not roll back only the image. Check the dashboard and restore $full_backup_archive with matching Core if needed."
  die "Core did not become ready at $ready_url"
fi

running_core="$(running_image "$main_service" || true)"
[ "$running_core" = "ghcr.io/srcfl/ftw:${target_tag}" ] || \
  die "running Core image is $running_core, want ghcr.io/srcfl/ftw:${target_tag}"

success=true
rmdir "$lock_dir"
trap - EXIT INT TERM

log "upgrade complete"
log "Core: $running_core"
log "updater: $(running_image ftw-updater)"
log "verified full backup: $full_backup_archive"
log "Do not use orange Update to jump 2.x → 3.x; this updater-first pair is the supported path."
