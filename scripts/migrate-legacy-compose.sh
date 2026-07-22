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
Usage: migrate-legacy-compose.sh --version vX.Y.Z[-beta.N] \
  --core-digest sha256:... --updater-digest sha256:... \
  [--dir PATH] [--backup-dir PATH]

Without --dir, the script uses the current directory when it contains
docker-compose.yml, then tries ~/ftw and ~/forty-two-watts.

The full .ftwbak archive is written outside the live data directory. Use a
mounted USB drive or another machine's mounted share for --backup-dir when
possible. The default is <installation>/ftw-backups.
EOF
}

requested_dir="${FTW_DIR:-}"
requested_full_backup_dir="${FTW_BACKUP_DIR:-}"
control_plane_version="${FTW_CONTROL_PLANE_VERSION:-}"
core_digest="${FTW_CONTROL_PLANE_CORE_DIGEST:-}"
updater_digest="${FTW_CONTROL_PLANE_UPDATER_DIGEST:-}"
while [ "$#" -gt 0 ]; do
  case "$1" in
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
    --version)
      [ "$#" -ge 2 ] || die "--version requires an immutable release tag"
      control_plane_version="$2"
      shift 2
      ;;
    --core-digest)
      [ "$#" -ge 2 ] || die "--core-digest requires a sha256 digest"
      core_digest="$2"
      shift 2
      ;;
    --updater-digest)
      [ "$#" -ge 2 ] || die "--updater-digest requires a sha256 digest"
      updater_digest="$2"
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

if [[ ! "$control_plane_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-beta\.[0-9]+)?$ ]]; then
  die "--version must be an immutable vX.Y.Z or vX.Y.Z-beta.N release"
fi
if [[ ! "$core_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  die "--core-digest must match the verified ftw-control-plane.json Core digest"
fi
if [[ ! "$updater_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  die "--updater-digest must match the verified ftw-control-plane.json updater digest"
fi

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

# Reuse the project identity already attached to any canonical/legacy
# container. This covers deployments originally launched with
# COMPOSE_PROJECT_NAME instead of the directory-derived default.
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

# container_name is optional. Locate generated Compose container names by
# their service + working-directory labels so a deployment started with an
# exported COMPOSE_PROJECT_NAME cannot be duplicated under the directory
# default during migration.
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
canonical_tags=false
if [ -n "$compose_project" ]; then
  compose_command+=(--project-name "$compose_project")
  log "preserving Compose project: $compose_project"
fi
compose() {
  if [ "$canonical_tags" = true ]; then
    FTW_IMAGE_TAG="$control_plane_version" FTW_UPDATER_IMAGE_TAG="$control_plane_version" FTW_OPTIMIZER_IMAGE_TAG=latest \
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
compose_backup_dir=""
full_backup_dir=""
full_backup_archive=""
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
previous_main_image_id=""
previous_main_image_ref=""
previous_updater_image_id=""
previous_updater_image_ref=""
previous_optimizer_image_id=""
previous_optimizer_image_ref=""
pinned_main_image_id=""
pinned_updater_image_id=""

restore_image_reference() {
  local image_id="$1"
  local image_ref="$2"
  [ -n "$image_id" ] && [ -n "$image_ref" ] || return 0
  case "$image_ref" in
    "<no value>"|sha256:*|*@sha256:*) return 0 ;;
  esac
  docker image tag "$image_id" "$image_ref"
}

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
  if [ -n "$compose_backup_dir" ] && [ -d "$compose_backup_dir" ]; then
    for file in "$compose_backup_dir"/*.yml "$compose_backup_dir"/*.yaml; do
      [ -f "$file" ] || continue
      if ! cp -p "$file" "$install_dir/$(basename "$file")"; then
        restore_ok=false
      fi
    done
  fi

  # A pull can move a mutable tag such as :latest before any container is
  # recreated. Put every previous immutable image back under its original
  # reference before Compose restores the old service definitions.
  if ! restore_image_reference "$previous_updater_image_id" "$previous_updater_image_ref"; then
    restore_ok=false
  fi
  if ! restore_image_reference "$previous_main_image_id" "$previous_main_image_ref"; then
    restore_ok=false
  fi
  if ! restore_image_reference "$previous_optimizer_image_id" "$previous_optimizer_image_ref"; then
    restore_ok=false
  fi

  # Restore updater first. A paired Core waits for its matching updater before
  # it opens state, so restoring Core first can leave a good rollback unready.
  if [ "$containers_changed" = true ]; then
    if ! compose up -d --no-deps --force-recreate ftw-updater >/dev/null 2>&1; then
      restore_ok=false
    fi
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
  if [ "$optimizer_container_changed" = true ] && [ "$optimizer_service_added" = false ]; then
    if ! compose up -d --no-deps --force-recreate ftw-optimizer >/dev/null 2>&1; then
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
  if [ "$optimizer_container_changed" = true ] && [ "$optimizer_service_added" = false ] && \
    [ -z "$(compose ps -q --status running ftw-optimizer 2>/dev/null)" ]; then
    restore_ok=false
  fi

  rmdir "$lock_dir" 2>/dev/null || restore_ok=false
  if [ "$restore_ok" = true ]; then
    printf '[FTW migration] Previous deployment and image references were restored. Data was not modified.\n' >&2
  else
    printf '[FTW migration] ERROR: automatic rollback was incomplete. Compose backups remain in %s; the verified full backup remains in %s.\n' "${compose_backup_dir:-<not-created>}" "${full_backup_archive:-<not-created>}" >&2
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

# Phase 1: make a portable, internally verified backup before changing a
# service definition or stopping a container. OpenBackupSource is read-only and
# deliberately performs no schema migration on the legacy database.
state_path="$expected_data_source/state.db"
[ -f "$state_path" ] || die "missing $state_path; use the manual backup procedure for a custom state.path"
log "phase 1/4: pulling the backup helper (the running deployment is unchanged)"
docker pull ghcr.io/srcfl/ftw:latest >/dev/null
backup_json="$(docker run --rm --user 0:0 \
  -v "$expected_data_source:/app/data:ro" \
  -v "$full_backup_dir:/backup" \
  --entrypoint /app/ftw-backup \
  ghcr.io/srcfl/ftw:latest \
  create -state /app/data/state.db -data /app/data -output /backup -core-version legacy)"
full_backup_id="$(printf '%s\n' "$backup_json" | sed -n 's/^[[:space:]]*"id": "\([^"]*\.ftwbak\)",*$/\1/p')"
case "$full_backup_id" in
  ""|*/*|*..*) die "backup helper returned an invalid archive id" ;;
esac
full_backup_archive="$full_backup_dir/$full_backup_id"
[ -f "$full_backup_archive" ] || die "backup helper did not publish $full_backup_archive"
docker run --rm --user 0:0 \
  -v "$full_backup_dir:/backup" \
  --entrypoint /app/ftw-backup \
  ghcr.io/srcfl/ftw:latest verify -archive "/backup/$full_backup_id" >/dev/null
# The archive is intentionally 0600. Give it back to the operator who invoked
# Docker so it can actually be copied off the host without root.
backup_owner_uid="${SUDO_UID:-$(id -u)}"
backup_owner_gid="${SUDO_GID:-$(id -g)}"
docker run --rm --user 0:0 \
  -v "$full_backup_dir:/backup" \
  --entrypoint chown \
  ghcr.io/srcfl/ftw:latest "$backup_owner_uid:$backup_owner_gid" "/backup/$full_backup_id"
log "verified full backup: $full_backup_archive"

compose_backup_dir="$install_dir/.ftw-migration-backup-$timestamp"
mkdir "$compose_backup_dir"

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
cp -p "${compose_files[@]}" "$compose_backup_dir/"
log "Compose rollback backup: $compose_backup_dir"

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

capture_service_image() {
  local service="$1"
  local container_id=""
  captured_image_id=""
  captured_image_ref=""
  container_id="$(compose ps -q --all "$service" | tail -n 1)"
  [ -n "$container_id" ] || return 0
  captured_image_id="$(docker inspect "$container_id" --format '{{.Image}}')"
  captured_image_ref="$(docker inspect "$container_id" --format '{{.Config.Image}}')"
}

# Keep immutable rollback points before pull can move any mutable image tag.
capture_service_image "$main_service"
previous_main_image_id="$captured_image_id"
previous_main_image_ref="$captured_image_ref"
capture_service_image ftw-updater
previous_updater_image_id="$captured_image_id"
previous_updater_image_ref="$captured_image_ref"
capture_service_image ftw-optimizer
previous_optimizer_image_id="$captured_image_id"
previous_optimizer_image_ref="$captured_image_ref"
printf '%s\t%s\t%s\n' \
  "$main_service" "$previous_main_image_id" "$previous_main_image_ref" \
  ftw-updater "$previous_updater_image_id" "$previous_updater_image_ref" \
  ftw-optimizer "$previous_optimizer_image_id" "$previous_optimizer_image_ref" \
  >"$compose_backup_dir/previous-images.tsv"

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
  rewrite_service_image "$file" 'ftw-updater' 'ghcr.io/srcfl/ftw-updater:${FTW_UPDATER_IMAGE_TAG:-latest}'
  rewrite_service_image "$file" 'ftw-optimizer' 'ghcr.io/srcfl/ftw-optimizer:${FTW_OPTIMIZER_IMAGE_TAG:-latest}'
done

# Pin both control-plane services to the chosen immutable release. Ignore a
# development tag from the caller's shell or an old .env file.
canonical_tags=true
compose config >/dev/null
effective_main_image="$(compose config --images "$main_service")"
effective_updater_image="$(compose config --images ftw-updater)"
effective_optimizer_image="$(compose config --images ftw-optimizer)"
[ "$effective_main_image" = "ghcr.io/srcfl/ftw:$control_plane_version" ] || \
  die "the effective $main_service image is not the selected release: $effective_main_image"
[ "$effective_updater_image" = "ghcr.io/srcfl/ftw-updater:$control_plane_version" ] || \
  die "the effective ftw-updater image is not the selected release: $effective_updater_image"
case "$effective_optimizer_image" in
  ghcr.io/srcfl/ftw-optimizer:*) ;;
  *) die "the effective ftw-optimizer image is not ghcr.io/srcfl/ftw-optimizer: $effective_optimizer_image" ;;
esac

core_digest_ref="ghcr.io/srcfl/ftw@$core_digest"
updater_digest_ref="ghcr.io/srcfl/ftw-updater@$updater_digest"
log "phase 2/4: pulling the digest-locked Core + updater control plane"
docker pull "$core_digest_ref" >/dev/null
docker pull "$updater_digest_ref" >/dev/null
pinned_main_image_id="$(docker image inspect "$core_digest_ref" --format '{{.Id}}')"
pinned_updater_image_id="$(docker image inspect "$updater_digest_ref" --format '{{.Id}}')"
[ -n "$pinned_main_image_id" ] || die "could not inspect the verified Core digest"
[ -n "$pinned_updater_image_id" ] || die "could not inspect the verified updater digest"
docker image tag "$core_digest_ref" "ghcr.io/srcfl/ftw:$control_plane_version"
docker image tag "$updater_digest_ref" "ghcr.io/srcfl/ftw-updater:$control_plane_version"

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

log "starting ftw-updater from ghcr.io/srcfl/ftw-updater:$control_plane_version"
containers_changed=true
compose up -d --no-deps --pull never --force-recreate ftw-updater
updater_id="$(compose ps -q --status running ftw-updater | tail -n 1)"
[ -n "$updater_id" ] || die "ftw-updater did not reach running state"
running_updater_image_id="$(docker inspect "$updater_id" --format '{{.Image}}')"
[ "$running_updater_image_id" = "$pinned_updater_image_id" ] || \
  die "running updater does not match the verified digest: $running_updater_image_id"

# v1.10.0-beta.1 cannot drive the new paired transaction. Its one-time bridge
# starts only the verified updater while old Core remains live, then replaces
# Core. Any loss of old Core health here triggers the existing image-ID restore.
health_url="${FTW_HEALTH_URL:-http://127.0.0.1:8080/api/health}"
ready_url="${FTW_READY_URL:-${health_url%/api/health}/api/status}"
curl -fsS --max-time 3 "$health_url" >/dev/null 2>&1 || \
  die "old Core lost health after the updater-first bootstrap"
log "old Core remained healthy after the updater-first bootstrap"

log "starting $main_service from ghcr.io/srcfl/ftw:$control_plane_version"
compose up -d --no-deps --pull never --force-recreate "$main_service"

new_main_id="$(compose ps -q --status running "$main_service" | tail -n 1)"
[ -n "$new_main_id" ] || die "$main_service did not reach running state"
updater_id="$(compose ps -q --status running ftw-updater | tail -n 1)"
[ -n "$updater_id" ] || die "ftw-updater did not reach running state"

running_main_image_id="$(docker inspect "$new_main_id" --format '{{.Image}}')"
running_updater_image_id="$(docker inspect "$updater_id" --format '{{.Image}}')"
[ "$running_main_image_id" = "$pinned_main_image_id" ] || \
  die "running Core does not match the verified digest: $running_main_image_id"
[ "$running_updater_image_id" = "$pinned_updater_image_id" ] || \
  die "running updater does not match the verified digest: $running_updater_image_id"

main_image="$(docker inspect "$new_main_id" --format '{{.Config.Image}}')"
updater_image="$(docker inspect "$updater_id" --format '{{.Config.Image}}')"
[ "$main_image" = "ghcr.io/srcfl/ftw:$control_plane_version" ] || \
  die "running main container is not the selected release: $main_image"
[ "$updater_image" = "ghcr.io/srcfl/ftw-updater:$control_plane_version" ] || \
  die "running updater is not the selected release: $updater_image"
running_data_source="$(docker inspect "$new_main_id" --format '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Source}}{{end}}{{end}}')"
if [ -d "$running_data_source" ]; then
  running_data_source="$(cd "$running_data_source" && pwd -P)"
fi
[ "$running_data_source" = "$expected_data_source" ] || \
  die "running main container uses unexpected /app/data source: $running_data_source"

healthy=false
health_timeout_s="${FTW_MIGRATION_HEALTH_TIMEOUT_S:-1800}"
health_deadline=$((SECONDS + health_timeout_s))
while [ "$SECONDS" -lt "$health_deadline" ]; do
  if curl -fsS --max-time 3 "$health_url" >/dev/null 2>&1 && \
    curl -fsS --max-time 3 "$ready_url" >/dev/null 2>&1; then
    healthy=true
    break
  fi
  sleep 2
done
[ "$healthy" = true ] || die "FTW did not finish initialization at $ready_url within ${health_timeout_s}s"

# Phase 3 deliberately starts only after Core + updater have passed their
# health gate. Optimizer has its own release and compatibility handshake; a
# failed optimizer must never roll back a healthy Core or touch persistent
# data. Core remains safe on its Go fallback while this phase is repaired.
optimizer_image="unavailable (Core is using its safe fallback)"
log "phase 3/4: updating Optimizer independently"
if compose pull ftw-optimizer && \
  compose up -d --no-deps --force-recreate ftw-optimizer; then
  optimizer_container_changed=true
  optimizer_id="$(compose ps -q --status running ftw-optimizer | tail -n 1)"
  optimizer_healthy=false
  if [ -n "$optimizer_id" ]; then
    for _ in $(seq 1 60); do
      optimizer_health="$(docker inspect "$optimizer_id" --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}unknown{{end}}' 2>/dev/null || true)"
      if [ "$optimizer_health" = healthy ]; then
        optimizer_healthy=true
        break
      fi
      sleep 2
    done
  fi
  if [ "$optimizer_healthy" = true ]; then
    optimizer_image="$(docker inspect "$optimizer_id" --format '{{.Config.Image}}')"
    case "$optimizer_image" in
      ghcr.io/srcfl/ftw-optimizer:*) ;;
      *) optimizer_healthy=false ;;
    esac
  fi
else
  optimizer_healthy=false
fi

if [ "${optimizer_healthy:-false}" != true ]; then
  log "WARNING: Optimizer did not become healthy; Core stays online on its Go fallback"
  if [ -n "$previous_optimizer_image_id" ]; then
    # Restore the old image bits under the effective optimizer reference only.
    # Core/updater and their Compose definitions remain committed.
    if docker image tag "$previous_optimizer_image_id" "$effective_optimizer_image" && \
      compose up -d --no-deps --force-recreate ftw-optimizer; then
      optimizer_id="$(compose ps -q --status running ftw-optimizer | tail -n 1)"
      if [ -n "$optimizer_id" ]; then
        optimizer_image="restored previous image ($previous_optimizer_image_ref)"
      fi
    else
      log "WARNING: previous Optimizer could not be restarted; Core remains healthy without it"
    fi
  else
    compose rm -s -f ftw-optimizer >/dev/null 2>&1 || true
  fi
fi

# Phase 4 refreshes signed metadata only. It does not activate or restart a
# driver; drivers are updated one at a time later from the Update Center.
log "phase 4/4: refreshing the signed driver catalog (no driver is activated)"
if curl -fsS --max-time 10 -X POST "${health_url%/api/health}/api/device_repository/refresh" >/dev/null 2>&1; then
  log "signed driver catalog refreshed"
else
  log "WARNING: driver catalog refresh failed; the running drivers are unchanged"
fi

success=true
rmdir "$lock_dir"
trap - EXIT INT TERM

log "migration complete"
log "main image: $main_image"
log "updater image: $updater_image"
log "optimizer image: $optimizer_image"
log "verified full backup: $full_backup_archive"
log "Compose rollback backup: $compose_backup_dir"
if [ -n "$renamed_container" ]; then
  log "container rollback backup: $renamed_container"
fi
log "data directory preserved: $expected_data_source"
