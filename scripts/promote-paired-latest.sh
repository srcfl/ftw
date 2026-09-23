#!/usr/bin/env bash
# Move the Core/updater latest channels as one recoverable operation.
#
# OCI registries cannot update tags in two repositories atomically. Capture all
# four old manifests before the first write. Restore touched aliases on a write
# failure or a publication failure that GitHub confirms left a draft. If the
# public state is unclear, stop without another mutation for operator audit.
# Versioned and sha-* tags are immutable and may remain exact or absent.

set -eEuo pipefail

: "${FTW_CORE_DIGEST:?FTW_CORE_DIGEST is required}"
: "${FTW_UPDATER_DIGEST:?FTW_UPDATER_DIGEST is required}"
: "${FTW_CANONICAL_GHCR_USER:?FTW_CANONICAL_GHCR_USER is required}"
: "${FTW_CANONICAL_GHCR_TOKEN:?FTW_CANONICAL_GHCR_TOKEN is required}"
: "${FTW_COMPATIBILITY_GHCR_USER:?FTW_COMPATIBILITY_GHCR_USER is required}"
: "${FTW_COMPATIBILITY_GHCR_TOKEN:?FTW_COMPATIBILITY_GHCR_TOKEN is required}"
: "${FTW_RELEASE_TAG:?FTW_RELEASE_TAG is required}"
: "${FTW_RELEASE_ID:?FTW_RELEASE_ID is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bash "${script_dir}/check-legacy-release-line.sh" "${FTW_RELEASE_TAG}"

docker_command="${FTW_DOCKER_COMMAND:-docker}"
inspect_command="${FTW_INSPECT_IMAGE_DIGEST:-scripts/inspect-image-digest.sh}"
gh_command="${FTW_GH_COMMAND:-gh}"
release_command="${FTW_RELEASE_COMMAND:-scripts/github-release-by-id.sh}"

scopes=(canonical canonical compatibility compatibility)
repositories=(
  ghcr.io/srcfl/ftw-updater
  ghcr.io/srcfl/ftw
  ghcr.io/frahlg/forty-two-watts-updater
  ghcr.io/frahlg/forty-two-watts
)
new_digests=(
  "${FTW_UPDATER_DIGEST}"
  "${FTW_CORE_DIGEST}"
  "${FTW_UPDATER_DIGEST}"
  "${FTW_CORE_DIGEST}"
)
old_digests=()
moved=()
publication_attempted=false

valid_digest() {
  [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]]
}

inspect_digest() {
  "${inspect_command}" "$1"
}

login_scope() {
  local scope="$1"
  local user token
  case "${scope}" in
    canonical)
      user="${FTW_CANONICAL_GHCR_USER}"
      token="${FTW_CANONICAL_GHCR_TOKEN}"
      ;;
    compatibility)
      user="${FTW_COMPATIBILITY_GHCR_USER}"
      token="${FTW_COMPATIBILITY_GHCR_TOKEN}"
      ;;
    *)
      echo "Unknown GHCR scope ${scope}." >&2
      return 1
      ;;
  esac
  printf '%s' "${token}" | "${docker_command}" login ghcr.io --username "${user}" --password-stdin >/dev/null
}

tag_digest() {
  local repository="$1"
  local digest="$2"
  local target="${repository}:latest"
  "${docker_command}" buildx imagetools create --prefer-index=false \
    --tag "${target}" "${repository}@${digest}"
  test "$(inspect_digest "${target}")" = "${digest}"
}

audit_publication() {
  release_state=unknown
  latest_state=unknown
  local release_json latest_tag

  if release_json="$("${release_command}" show "${FTW_RELEASE_ID}")"; then
    if jq -e --arg tag "${FTW_RELEASE_TAG}" '
      .tag_name == $tag
      and .draft == false
      and .prerelease == false
      and (.published_at | type == "string" and length > 0)
    ' <<<"${release_json}" >/dev/null; then
      release_state=public
    elif jq -e --arg tag "${FTW_RELEASE_TAG}" '
      .tag_name == $tag
      and .draft == true
      and .prerelease == false
      and .published_at == null
    ' <<<"${release_json}" >/dev/null; then
      release_state=draft
    fi
  fi

  if latest_tag="$("${gh_command}" api \
    "repos/${GITHUB_REPOSITORY}/releases/latest" --jq '.tag_name')"; then
    if [ "${latest_tag}" = "${FTW_RELEASE_TAG}" ]; then
      latest_state=candidate
    else
      latest_state=other
    fi
  fi
  return 0
}

# A missing or unreadable old alias cannot be restored, so stop before any
# mutation. This also separates a registry observation failure from a service
# failure: the latter never gets to change a release channel.
for i in "${!repositories[@]}"; do
  alias="${repositories[$i]}:latest"
  if ! old="$(inspect_digest "${alias}")" || ! valid_digest "${old}"; then
    echo "Cannot capture restorable digest for ${alias}; refusing to move any latest alias." >&2
    exit 1
  fi
  if ! restorable="$(inspect_digest "${repositories[$i]}@${old}")" || [ "${restorable}" != "${old}" ]; then
    echo "Cannot read rollback source ${repositories[$i]}@${old}; refusing to move any latest alias." >&2
    exit 1
  fi
  if ! valid_digest "${new_digests[$i]}"; then
    echo "Invalid candidate digest for ${alias}: ${new_digests[$i]}." >&2
    exit 1
  fi
  old_digests+=("${old}")
done
if [ "${old_digests[0]}" != "${old_digests[2]}" ] || \
   [ "${old_digests[1]}" != "${old_digests[3]}" ]; then
  echo "Canonical and compatibility latest aliases are already out of sync; refusing to move them." >&2
  exit 1
fi

rollback() {
  local original_status="$1"
  local rollback_failed=false
  local active_scope=""
  trap '' INT TERM
  trap - ERR
  set +e

  echo "Latest alias promotion failed; restoring every alias already touched." >&2
  for ((position=${#moved[@]}-1; position>=0; position--)); do
    i="${moved[$position]}"
    if [ "${scopes[$i]}" != "${active_scope}" ]; then
      active_scope="${scopes[$i]}"
      if ! login_scope "${active_scope}"; then
        echo "Could not log in to restore ${active_scope} aliases." >&2
        rollback_failed=true
        continue
      fi
    fi
    if ! tag_digest "${repositories[$i]}" "${old_digests[$i]}"; then
      echo "Could not restore ${repositories[$i]}:latest to ${old_digests[$i]}." >&2
      rollback_failed=true
    fi
  done

  if [ "${rollback_failed}" = true ]; then
    echo "Latest alias rollback was incomplete; release channels need operator repair." >&2
    exit 70
  fi
  exit "${original_status}"
}

handle_signal() {
  local signal_name="$1"
  local signal_status="$2"
  trap '' INT TERM
  trap - ERR
  set +e

  audit_publication
  if [ "${release_state}" = public ] && [ "${latest_state}" = candidate ]; then
    echo "Signal ${signal_name} arrived after publication; Core/updater latest aliases and public release match."
    exit 0
  fi
  if [ "${publication_attempted}" = false ] && \
     [ "${release_state}" = draft ] && \
     [ "${latest_state}" = other ]; then
    rollback "${signal_status}"
  fi

  echo "Signal ${signal_name} left publication state ${release_state}+${latest_state}; aliases were not changed again and operator audit is required." >&2
  exit 71
}

# A failed publish job can be rerun without rerunning its successful meta job.
# Refresh the public release inventory at the last safe point so an abandoned
# old draft cannot move latest after a newer stable has shipped.
release_pages="$("${gh_command}" api --paginate --slurp \
  "repos/${GITHUB_REPOSITORY}/releases?per_page=100")"
printf '%s' "${release_pages}" | \
  python3 scripts/check-stable-release.py order "${FTW_RELEASE_TAG}"

trap 'rollback "$?"' ERR
trap 'handle_signal INT 130' INT
trap 'handle_signal TERM 143' TERM

active_scope=""
for i in "${!repositories[@]}"; do
  if [ "${scopes[$i]}" != "${active_scope}" ]; then
    active_scope="${scopes[$i]}"
    login_scope "${active_scope}"
  fi
  # Record intent before the registry call. If the client reports an error
  # after the registry accepted it, rollback still restores this alias.
  moved+=("${i}")
  tag_digest "${repositories[$i]}" "${new_digests[$i]}"
done

# The edit can succeed remotely and still return an error. Audit both GitHub
# views before deciding whether rollback is safe. Once public state is proved,
# never move aliases back; an unclear public/latest result needs operator audit.
edit_status=0
publication_attempted=true
if "${release_command}" publish "${FTW_RELEASE_ID}" >/dev/null; then
  edit_status=0
else
  edit_status=$?
fi
audit_publication

if [ "${release_state}" = public ]; then
  trap '' INT TERM
  trap - ERR
  if [ "${latest_state}" = candidate ]; then
    echo "Core/updater latest aliases and public release now match."
    exit 0
  fi
  echo "Release ${FTW_RELEASE_TAG} is public but latest state is ${latest_state}; operator audit required." >&2
  exit 71
fi
if [ "${edit_status}" -ne 0 ] && \
   [ "${release_state}" = draft ] && \
   [ "${latest_state}" = other ]; then
  rollback "${edit_status}"
fi

trap '' INT TERM
trap - ERR
echo "Could not prove a coherent publication state for ${FTW_RELEASE_TAG}; aliases were not changed again and operator audit is required." >&2
exit 71
