#!/usr/bin/env bash

# Migrate an existing Docker Compose installation to the canonical Sourceful
# images without renaming its directory, Compose project, main service, or data
# bind. Existing Compose files and any unmanaged main container are retained as
# rollback backups.

set -Eeuo pipefail

umask 077

log() {
  printf '[FTW migration] %s\n' "$*"
}

die() {
  printf '[FTW migration] ERROR: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: migrate-legacy-compose.sh [--dir PATH]

Without --dir, the script uses the current directory when it contains
docker-compose.yml, then tries ~/ftw and ~/forty-two-watts.
EOF
}

requested_dir="${FTW_DIR:-}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --dir)
      [ "$#" -ge 2 ] || die "--dir requires a path"
      requested_dir="$2"
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

[ "$(uname -s)" = Linux ] || die "automatic migration currently supports Linux Docker hosts only"
command -v docker >/dev/null 2>&1 || die "docker is not installed"
command -v curl >/dev/null 2>&1 || die "curl is not installed"
docker compose version >/dev/null 2>&1 || die "docker compose is not available"
docker info >/dev/null 2>&1 || \
  die "cannot access the Docker daemon; log out and in after joining the docker group, then retry"

if [ -n "$requested_dir" ]; then
  install_dir="$requested_dir"
elif [ -f "$PWD/docker-compose.yml" ]; then
  install_dir="$PWD"
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

# Reuse the project identity already attached to any canonical/legacy
# container. This covers deployments originally launched with
# COMPOSE_PROJECT_NAME instead of the directory-derived default.
compose_project=""
for known_container in ftw forty-two-watts ftw-updater; do
  if ! docker container inspect "$known_container" >/dev/null 2>&1; then
    continue
  fi
  candidate_project="$(docker inspect "$known_container" --format '{{index .Config.Labels "com.docker.compose.project"}}')"
  case "$candidate_project" in
    ""|"<no value>") continue ;;
  esac
  if [ -n "$compose_project" ] && [ "$candidate_project" != "$compose_project" ]; then
    die "existing FTW containers belong to different Compose projects"
  fi
  compose_project="$candidate_project"
done

compose_command=(docker compose)
canonical_tags=false
if [ -n "$compose_project" ]; then
  compose_command+=(--project-name "$compose_project")
  log "preserving Compose project: $compose_project"
fi
compose() {
  if [ "$canonical_tags" = true ]; then
    FTW_IMAGE_TAG=latest FTW_UPDATER_IMAGE_TAG=latest FTW_OPTIMIZER_IMAGE_TAG=latest \
      "${compose_command[@]}" "$@"
  else
    "${compose_command[@]}" "$@"
  fi
}

lock_dir="$install_dir/.ftw-migration.lock"
if ! mkdir "$lock_dir" 2>/dev/null; then
  die "another migration may be active ($lock_dir exists)"
fi

success=false
backup_dir=""
renamed_container=""
renamed_container_was_running=false
main_service=""
containers_changed=false
config_check=""
expected_data_source=""
new_main_id=""
optimizer_service_added=false
optimizer_container_changed=false
modular_override_created=""
modular_tmp=""
optimizer_id=""

restore_after_failure() {
  status=$?
  restore_ok=true
  if [ -n "$config_check" ]; then
    rm -f "$config_check"
  fi
  if [ "$success" = true ] || [ "$status" -eq 0 ]; then
    return
  fi

  printf '[FTW migration] Migration failed; restoring the previous deployment.\n' >&2
  canonical_tags=false
  if [ "$optimizer_container_changed" = true ] && [ -n "$modular_override_created" ]; then
    if ! compose rm -s -f ftw-optimizer >/dev/null 2>&1; then
      restore_ok=false
    fi
  fi
  if [ -n "$modular_tmp" ]; then
    if ! rm -f "$modular_tmp"; then
      restore_ok=false
    fi
  fi
  if [ -n "$modular_override_created" ]; then
    if ! rm -f "$modular_override_created"; then
      restore_ok=false
    fi
  fi
  if [ -n "$backup_dir" ] && [ -d "$backup_dir" ]; then
    for file in "$backup_dir"/*.yml "$backup_dir"/*.yaml; do
      [ -f "$file" ] || continue
      if ! cp -p "$file" "$install_dir/$(basename "$file")"; then
        restore_ok=false
      fi
    done
  fi

  if [ -n "$renamed_container" ]; then
    # Remove any replacement by Compose service identity as well as by exact
    # ID/name. A Compose file without container_name uses a generated name.
    if ! compose rm -s -f "$main_service" >/dev/null 2>&1; then
      restore_ok=false
    fi
    if [ -n "$new_main_id" ] && docker container inspect "$new_main_id" >/dev/null 2>&1; then
      if ! docker rm -f "$new_main_id" >/dev/null 2>&1; then
        restore_ok=false
      fi
    fi
    if docker container inspect "$main_service" >/dev/null 2>&1; then
      if ! docker rm -f "$main_service" >/dev/null 2>&1; then
        restore_ok=false
      fi
    fi
    if ! docker container inspect "$renamed_container" >/dev/null 2>&1; then
      restore_ok=false
    elif ! docker rename "$renamed_container" "$main_service" >/dev/null 2>&1; then
      restore_ok=false
    elif [ "$renamed_container_was_running" = true ] && ! docker start "$main_service" >/dev/null 2>&1; then
      restore_ok=false
    fi
  elif [ "$containers_changed" = true ] && [ -n "$main_service" ]; then
    if ! compose up -d --no-deps --force-recreate "$main_service" >/dev/null 2>&1; then
      restore_ok=false
    fi
  fi
  if [ "$containers_changed" = true ]; then
    if ! compose up -d --no-deps --force-recreate ftw-updater >/dev/null 2>&1; then
      restore_ok=false
    fi
  fi

  if [ -n "$renamed_container" ]; then
    if ! docker container inspect "$main_service" >/dev/null 2>&1; then
      restore_ok=false
    elif [ "$renamed_container_was_running" = true ] && \
      [ "$(docker inspect "$main_service" --format '{{.State.Running}}')" != true ]; then
      restore_ok=false
    fi
  elif [ "$containers_changed" = true ] && [ -n "$main_service" ]; then
    if [ -z "$(compose ps -q --status running "$main_service" 2>/dev/null)" ]; then
      restore_ok=false
    fi
  fi
  if [ "$containers_changed" = true ] && \
    [ -z "$(compose ps -q --status running ftw-updater 2>/dev/null)" ]; then
    restore_ok=false
  fi

  rmdir "$lock_dir" 2>/dev/null || restore_ok=false
  if [ "$restore_ok" = true ]; then
    printf '[FTW migration] Previous Compose files and container were restored. Data was not modified.\n' >&2
  else
    printf '[FTW migration] ERROR: automatic rollback was incomplete. Compose backups remain in %s; data was not modified.\n' "${backup_dir:-<not-created>}" >&2
  fi
  return "$status"
}
trap restore_after_failure EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

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
  die "both ftw and forty-two-watts services exist; refusing an ambiguous migration"
elif [ "$has_ftw" = true ]; then
  main_service="ftw"
elif [ "$has_legacy" = true ]; then
  main_service="forty-two-watts"
else
  die "no ftw or forty-two-watts main service found"
fi

if ! printf '%s\n' "$services" | grep -qx 'ftw-updater'; then
  die "ftw-updater is missing; this layout needs manual review"
fi

if ! printf '%s\n' "$services" | grep -qx 'ftw-optimizer'; then
  optimizer_service_added=true
  for candidate in \
    docker-compose.override.yml \
    docker-compose.override.yaml \
    compose.override.yml \
    compose.override.yaml; do
    if [ -e "$candidate" ]; then
      die "ftw-optimizer is missing and $candidate already exists; merge the modular service into that override manually"
    fi
  done
fi

config_check="$(mktemp)"
# Scope the mount check to the selected main service. A global grep could be
# fooled by an unrelated helper service that happens to mount /app/data.
compose config "$main_service" >"$config_check"
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
[ "$data_mount_type" = bind ] || die "the /app/data mount must be a host bind for automatic migration"
[ -d "$data_mount_source" ] || die "the /app/data source does not exist: $data_mount_source"
expected_data_source="$(cd "$data_mount_source" && pwd -P)"
rm -f "$config_check"
config_check=""

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup_dir="$install_dir/.ftw-migration-backup-$timestamp"
mkdir "$backup_dir"

compose_files=(docker-compose.yml)
for candidate in \
  docker-compose.override.yml \
  docker-compose.override.yaml \
  compose.override.yml \
  compose.override.yaml; do
  if [ -f "$candidate" ]; then
    compose_files+=("$candidate")
  fi
done
cp -p "${compose_files[@]}" "$backup_dir/"
log "backup: $backup_dir"

if [ "$optimizer_service_added" = true ]; then
  modular_override_created="$install_dir/docker-compose.override.yml"
  modular_tmp="$modular_override_created.tmp"
  {
    echo '# Generated by scripts/migrate-legacy-compose.sh'
    echo 'services:'
    echo "  ${main_service}:"
    echo '    environment:'
    echo '      FTW_OPTIMIZER_TRANSPORT: ${FTW_OPTIMIZER_TRANSPORT:-auto}'
    echo '      FTW_OPTIMIZER_SOCKET: /run/ftw-optimizer/optimizer.sock'
    echo '    volumes:'
    echo '      - optimizer-ipc:/run/ftw-optimizer'
    echo '  ftw-optimizer:'
    echo '    image: ghcr.io/srcfl/ftw-optimizer:${FTW_OPTIMIZER_IMAGE_TAG:-latest}'
    echo '    container_name: ftw-optimizer'
    echo '    restart: unless-stopped'
    echo '    network_mode: none'
    echo '    environment:'
    echo '      FTW_OPTIMIZER_SOCKET: /run/ftw-optimizer/optimizer.sock'
    echo '    volumes:'
    echo '      - optimizer-ipc:/run/ftw-optimizer'
    echo 'volumes:'
    echo '  optimizer-ipc:'
  } >"$modular_tmp"
  docker compose -f "$compose_file" -f "$modular_tmp" config >/dev/null
  mv "$modular_tmp" "$modular_override_created"
  modular_tmp=""
  compose_files+=(docker-compose.override.yml)
  log "added modular optimizer override: $modular_override_created"
fi

rewrite_service_image() {
  file="$1"
  service="$2"
  image="$3"
  tmp="$(mktemp "$install_dir/.ftw-compose.XXXXXX")"

  if ! awk -v wanted_service="$service" -v wanted_image="$image" '
    function indentation(value) {
      match(value, /^[[:space:]]*/)
      return RLENGTH
    }
    function content(value) {
      sub(/^[[:space:]]*/, "", value)
      return value
    }
    BEGIN {
      in_services = 0
      in_service = 0
      services_indent = -1
      service_indent = -1
      property_indent = -1
      replacements = 0
    }
    {
      line = $0
      stripped = content(line)
      indent = indentation(line)

      if (stripped ~ /^services:[[:space:]]*(#.*)?$/) {
        in_services = 1
        in_service = 0
        services_indent = indent
        print line
        next
      }

      if (in_services && stripped !~ /^(#.*)?$/ && indent <= services_indent) {
        in_services = 0
        in_service = 0
      }

      service_header = stripped
      sub(/[[:space:]]*#.*/, "", service_header)
      if (in_services && service_header == wanted_service ":") {
        in_service = 1
        service_indent = indent
        property_indent = -1
        print line
        next
      }

      if (in_service && stripped !~ /^(#.*)?$/ && indent <= service_indent) {
        in_service = 0
      }

      if (in_service && property_indent < 0 && stripped !~ /^(#.*)?$/ && indent > service_indent) {
        property_indent = indent
      }

      if (in_service && indent == property_indent && stripped ~ /^image:[[:space:]]*/) {
        prefix = substr(line, 1, indent)
        print prefix "image: " wanted_image
        replacements++
        next
      }

      print line
    }
    END {
      if (replacements > 1) exit 42
    }
  ' "$file" >"$tmp"; then
    rm -f "$tmp"
    die "could not safely rewrite $service image in $file"
  fi

  chmod --reference="$file" "$tmp"
  mv "$tmp" "$file"
}

for file in "${compose_files[@]}"; do
  rewrite_service_image "$file" "$main_service" 'ghcr.io/srcfl/ftw:${FTW_IMAGE_TAG:-latest}'
  rewrite_service_image "$file" 'ftw-updater' 'ghcr.io/srcfl/ftw-updater:latest'
done

# A caller's shell or old .env file may contain a development tag. Use the
# canonical stable tag for this one-time migration; future in-app updates pass
# their own immutable FTW_IMAGE_TAG to Compose.
canonical_tags=true
compose config >/dev/null
effective_main_image="$(compose config --images "$main_service")"
effective_updater_image="$(compose config --images ftw-updater)"
effective_optimizer_image="$(compose config --images ftw-optimizer)"
case "$effective_main_image" in
  ghcr.io/srcfl/ftw:*) ;;
  *) die "the effective $main_service image is not ghcr.io/srcfl/ftw: $effective_main_image" ;;
esac
case "$effective_updater_image" in
  ghcr.io/srcfl/ftw-updater:*) ;;
  *) die "the effective ftw-updater image is not ghcr.io/srcfl/ftw-updater: $effective_updater_image" ;;
esac
case "$effective_optimizer_image" in
  ghcr.io/srcfl/ftw-optimizer:*) ;;
  *) die "the effective ftw-optimizer image is not ghcr.io/srcfl/ftw-optimizer: $effective_optimizer_image" ;;
esac

log "pulling canonical Sourceful images"
pull_services=("$main_service" ftw-updater)
if [ "$optimizer_service_added" = true ]; then
  pull_services+=(ftw-optimizer)
fi
compose pull "${pull_services[@]}"

# Some developer installations replaced the Compose-managed main container
# with a manually created container of the same name. Preserve it as a stopped
# rollback artifact before Compose takes ownership again.
if docker container inspect "$main_service" >/dev/null 2>&1; then
  container_project="$(docker inspect "$main_service" --format '{{index .Config.Labels "com.docker.compose.project"}}')"
  if [ -z "$container_project" ] || [ "$container_project" = "<no value>" ]; then
    actual_data="$(docker inspect "$main_service" --format '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Source}}{{end}}{{end}}')"
    if [ -d "$actual_data" ]; then
      actual_data="$(cd "$actual_data" && pwd -P)"
    fi
    if [ "$actual_data" != "$expected_data_source" ]; then
      die "unmanaged $main_service container uses unexpected data path: $actual_data"
    fi
    if [ "$(docker inspect "$main_service" --format '{{.State.Running}}')" = true ]; then
      renamed_container_was_running=true
    fi
    renamed_container="${main_service}-legacy-backup-$timestamp"
    # Docker can rename a running container. Record the rollback identity
    # before stopping it so a failed stop cannot leave an untracked, stopped
    # legacy service behind.
    docker rename "$main_service" "$renamed_container"
    if [ "$renamed_container_was_running" = true ]; then
      docker stop --time 30 "$renamed_container" >/dev/null
    fi
    containers_changed=true
    log "preserved previous container as $renamed_container"
  fi
fi

if [ "$optimizer_service_added" = true ]; then
  log "starting ftw-optimizer from ghcr.io/srcfl/ftw-optimizer"
  optimizer_container_changed=true
  compose up -d --no-deps --force-recreate ftw-optimizer
fi

log "starting $main_service from ghcr.io/srcfl/ftw"
containers_changed=true
compose up -d --no-deps --force-recreate "$main_service"
log "starting ftw-updater from ghcr.io/srcfl/ftw-updater"
compose up -d --no-deps --force-recreate ftw-updater

new_main_id="$(compose ps -q --status running "$main_service" | tail -n 1)"
[ -n "$new_main_id" ] || die "$main_service did not reach running state"
updater_id="$(compose ps -q --status running ftw-updater | tail -n 1)"
[ -n "$updater_id" ] || die "ftw-updater did not reach running state"
optimizer_id="$(compose ps -q --status running ftw-optimizer | tail -n 1)"
[ -n "$optimizer_id" ] || die "ftw-optimizer did not reach running state"

main_image="$(docker inspect "$new_main_id" --format '{{.Config.Image}}')"
updater_image="$(docker inspect "$updater_id" --format '{{.Config.Image}}')"
optimizer_image="$(docker inspect "$optimizer_id" --format '{{.Config.Image}}')"
case "$main_image" in
  ghcr.io/srcfl/ftw:*) ;;
  *) die "running main container uses unexpected image: $main_image" ;;
esac
case "$updater_image" in
  ghcr.io/srcfl/ftw-updater:*) ;;
  *) die "running updater uses unexpected image: $updater_image" ;;
esac
case "$optimizer_image" in
  ghcr.io/srcfl/ftw-optimizer:*) ;;
  *) die "running optimizer uses unexpected image: $optimizer_image" ;;
esac

running_data_source="$(docker inspect "$new_main_id" --format '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Source}}{{end}}{{end}}')"
if [ -d "$running_data_source" ]; then
  running_data_source="$(cd "$running_data_source" && pwd -P)"
fi
[ "$running_data_source" = "$expected_data_source" ] || \
  die "running main container uses unexpected /app/data source: $running_data_source"

health_url="${FTW_HEALTH_URL:-http://127.0.0.1:8080/api/health}"
healthy=false
for _ in $(seq 1 60); do
  if curl -fsS --max-time 3 "$health_url" >/dev/null 2>&1; then
    healthy=true
    break
  fi
  sleep 2
done
[ "$healthy" = true ] || die "FTW did not answer at $health_url within 120 seconds"

success=true
rmdir "$lock_dir"
trap - EXIT INT TERM

log "migration complete"
log "main image: $main_image"
log "updater image: $updater_image"
log "optimizer image: $optimizer_image"
log "Compose backup: $backup_dir"
if [ -n "$renamed_container" ]; then
  log "container rollback backup: $renamed_container"
fi
log "data directory preserved: $expected_data_source"
