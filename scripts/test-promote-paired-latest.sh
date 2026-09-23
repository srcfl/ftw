#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
subject="${root}/scripts/promote-paired-latest.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

state="${tmp}/state"
log="${tmp}/calls.log"
final_state="${tmp}/published"
mkdir -p "${tmp}/bin"

cat > "${tmp}/inspect" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == *@sha256:* ]]; then
  if [ -n "${FTW_FAKE_UNREADABLE_REF:-}" ] && [ "$1" = "${FTW_FAKE_UNREADABLE_REF}" ]; then
    exit 1
  fi
  printf '%s\n' "${1##*@}"
  exit 0
fi
awk -F '|' -v alias="$1" '$1 == alias { print $2; found=1 } END { exit !found }' "${FTW_FAKE_STATE}"
SH

cat > "${tmp}/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [ "$1" = login ]; then
  cat >/dev/null
  printf 'login|%s\n' "$*" >> "${FTW_FAKE_LOG}"
  exit 0
fi
if [ "$1 $2 $3" != "buildx imagetools create" ]; then
  echo "unexpected docker call: $*" >&2
  exit 2
fi
target=""
source="${*: -1}"
for ((i=1; i<=$#; i++)); do
  if [ "${!i}" = --tag ]; then
    next=$((i + 1))
    target="${!next}"
    break
  fi
done
digest="${source##*@}"
printf 'tag|%s|%s\n' "${target}" "${digest}" >> "${FTW_FAKE_LOG}"
if [ -n "${FTW_FAKE_FAIL_TARGET:-}" ] && [ "${target}" = "${FTW_FAKE_FAIL_TARGET}" ] && [ "${digest}" != "${FTW_FAKE_FAIL_OLD_DIGEST:-}" ]; then
  exit 1
fi
if [ -n "${FTW_FAKE_FAIL_RESTORE_TARGET:-}" ] && \
   [ "${target}" = "${FTW_FAKE_FAIL_RESTORE_TARGET}" ] && \
   [ "${digest}" = "${FTW_FAKE_FAIL_RESTORE_DIGEST:-}" ]; then
  exit 1
fi
awk -F '|' -v alias="${target}" '$1 != alias' "${FTW_FAKE_STATE}" > "${FTW_FAKE_STATE}.tmp"
printf '%s|%s\n' "${target}" "${digest}" >> "${FTW_FAKE_STATE}.tmp"
mv "${FTW_FAKE_STATE}.tmp" "${FTW_FAKE_STATE}"
if [ -n "${FTW_FAKE_SIGNAL_NAME:-}" ] && \
   [ "${target}" = "${FTW_FAKE_SIGNAL_TARGET:-}" ] && \
   [ "${digest}" = "${FTW_FAKE_SIGNAL_DIGEST:-}" ]; then
  printf 'signal|%s|%s\n' "${FTW_FAKE_SIGNAL_NAME}" "${target}" >> "${FTW_FAKE_LOG}"
  kill -s "${FTW_FAKE_SIGNAL_NAME}" "${PPID}"
fi
if [ -n "${FTW_FAKE_SECOND_SIGNAL_NAME:-}" ] && \
   [ "${target}" = "${FTW_FAKE_SECOND_SIGNAL_TARGET:-}" ] && \
   [ "${digest}" = "${FTW_FAKE_SECOND_SIGNAL_DIGEST:-}" ]; then
  printf 'signal|%s|%s\n' "${FTW_FAKE_SECOND_SIGNAL_NAME}" "${target}" >> "${FTW_FAKE_LOG}"
  kill -s "${FTW_FAKE_SECOND_SIGNAL_NAME}" "${PPID}"
fi
SH

cat > "${tmp}/bin/gh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf 'gh|%s\n' "$*" >> "${FTW_FAKE_LOG}"
if [ "$1" = api ]; then
  public_stable="${FTW_FAKE_PUBLIC_STABLE:-v1.0.0}"
  if [[ "$*" == *'releases?per_page=100'* ]]; then
    printf '[[{"tag_name":"%s","draft":false,"prerelease":false,"published_at":"2026-08-17T00:00:00Z"}]]\n' "${public_stable}"
    exit 0
  fi
  if [ "${FTW_FAKE_FAIL_LATEST:-false}" = true ]; then
    exit 1
  fi
  if [ -n "${FTW_FAKE_LATEST_TAG:-}" ]; then
    printf '%s\n' "${FTW_FAKE_LATEST_TAG}"
  elif [ -f "${FTW_FAKE_FINAL_STATE}" ]; then
    printf '%s\n' "${FTW_RELEASE_TAG}"
  else
    printf '%s\n' "${public_stable}"
  fi
  exit 0
fi
echo "unexpected gh call: $*" >&2
exit 2
SH

cat > "${tmp}/release" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf 'release|%s\n' "$*" >> "${FTW_FAKE_LOG}"
if [ "$1" = publish ]; then
  if [ "${FTW_FAKE_FAIL_BEFORE_EDIT:-false}" = true ]; then
    exit 1
  fi
  if [ "${FTW_FAKE_SUCCESS_WITH_STALE_DRAFT:-false}" = true ]; then
    exit 0
  fi
  printf 'published\n' > "${FTW_FAKE_FINAL_STATE}"
  if [ "${FTW_FAKE_FAIL_AFTER_EDIT:-false}" = true ]; then
    exit 1
  fi
  exit 0
fi
if [ "$1" = show ]; then
  if [ "${FTW_FAKE_FAIL_VIEW:-false}" = true ]; then
    exit 1
  fi
  if [ -f "${FTW_FAKE_FINAL_STATE}" ]; then
    printf '{"tag_name":"%s","draft":false,"prerelease":false,"published_at":"2026-08-17T00:00:00Z"}\n' "${FTW_RELEASE_TAG}"
  else
    printf '{"tag_name":"%s","draft":true,"prerelease":false,"published_at":null}\n' "${FTW_RELEASE_TAG}"
  fi
  exit 0
fi
echo "unexpected release call: $*" >&2
exit 2
SH
chmod +x "${tmp}/inspect" "${tmp}/bin/docker"
chmod +x "${tmp}/bin/gh" "${tmp}/release"

old_core="sha256:$(printf '1%.0s' {1..64})"
old_updater="sha256:$(printf '2%.0s' {1..64})"
old_compat_core="${old_core}"
old_compat_updater="${old_updater}"
new_core="sha256:$(printf 'a%.0s' {1..64})"
new_updater="sha256:$(printf 'b%.0s' {1..64})"
newer_core="sha256:$(printf 'c%.0s' {1..64})"
newer_updater="sha256:$(printf 'd%.0s' {1..64})"

write_initial_state() {
  cat > "${state}" <<EOF
ghcr.io/srcfl/ftw:latest|${old_core}
ghcr.io/srcfl/ftw-updater:latest|${old_updater}
ghcr.io/frahlg/forty-two-watts:latest|${old_compat_core}
ghcr.io/frahlg/forty-two-watts-updater:latest|${old_compat_updater}
EOF
  : > "${log}"
  rm -f "${final_state}"
}

run_subject() {
  # Bash 3.2 treats an empty-array expansion as unbound under `set -u`.
  local -a overrides=(FTW_TEST_OVERRIDE_SENTINEL=1)
  while [ "$#" -gt 0 ] && [[ "$1" == *=* ]]; do
    overrides+=("$1")
    shift
  done
  if [ "$#" -ne 0 ]; then
    echo "run_subject accepts environment overrides only" >&2
    exit 2
  fi
  env \
    PATH="${tmp}/bin:${PATH}" \
    FTW_FAKE_STATE="${state}" \
    FTW_FAKE_LOG="${log}" \
    FTW_FAKE_FINAL_STATE="${final_state}" \
    FTW_INSPECT_IMAGE_DIGEST="${tmp}/inspect" \
    FTW_GH_COMMAND="${tmp}/bin/gh" \
    FTW_RELEASE_COMMAND="${tmp}/release" \
    FTW_CORE_DIGEST="${new_core}" \
    FTW_UPDATER_DIGEST="${new_updater}" \
    FTW_RELEASE_TAG=v2.0.0 \
    FTW_RELEASE_ID=123456 \
    GITHUB_REPOSITORY=srcfl/ftw \
    GH_TOKEN=test-token \
    FTW_CANONICAL_GHCR_USER=canonical \
    FTW_CANONICAL_GHCR_TOKEN=canonical-token \
    FTW_COMPATIBILITY_GHCR_USER=compatibility \
    FTW_COMPATIBILITY_GHCR_TOKEN=compatibility-token \
    "${overrides[@]}" \
    "${subject}"
}

assert_digest() {
  local alias="$1"
  local expected="$2"
  local actual
  actual="$(FTW_FAKE_STATE="${state}" "${tmp}/inspect" "${alias}")"
  if [ "${actual}" != "${expected}" ]; then
    echo "${alias} = ${actual}, want ${expected}" >&2
    exit 1
  fi
}

for allowed_tag in v2.3.3 v2.3.3-beta.1; do
  bash scripts/check-legacy-release-line.sh "${allowed_tag}"
done

# A recovery run checks out the release helpers beside the old tag's files.
# The guard must come from the helper checkout, not from the working directory.
mkdir -p "${tmp}/release-workflow/scripts"
cp "${subject}" "${tmp}/release-workflow/scripts/promote-paired-latest.sh"
cp "${root}/scripts/check-legacy-release-line.sh" "${tmp}/release-workflow/scripts/check-legacy-release-line.sh"
subject="${tmp}/release-workflow/scripts/promote-paired-latest.sh"
write_initial_state
guard_error="$(cd "${tmp}" && run_subject FTW_RELEASE_TAG=v3.8.0 2>&1)" && {
  echo "recovery helper allowed a cross-line release" >&2
  exit 1
}
if [[ "${guard_error}" != *"cannot use the legacy Docker release channel"* ]]; then
  echo "recovery helper did not find its own release-line guard" >&2
  exit 1
fi
if [ -s "${log}" ] || [ -e "${final_state}" ]; then
  echo "recovery helper changed release state before its line check" >&2
  exit 1
fi
subject="${root}/scripts/promote-paired-latest.sh"

# Another Docker major or native 0.x release must not touch old latest aliases.
for blocked_tag in v1.9.9 v3.8.0 v0.131.0; do
  write_initial_state
  if run_subject "FTW_RELEASE_TAG=${blocked_tag}"; then
    echo "${blocked_tag} unexpectedly entered the legacy latest channel" >&2
    exit 1
  fi
  if [ -s "${log}" ] || [ -e "${final_state}" ]; then
    echo "${blocked_tag} changed release state before its line check" >&2
    exit 1
  fi
  assert_digest ghcr.io/srcfl/ftw:latest "${old_core}"
  assert_digest ghcr.io/srcfl/ftw-updater:latest "${old_updater}"
done

# Fail after updater canonical latest moved. The trap must restore updater and leave
# every other member of both namespaces at its captured digest.
write_initial_state
if run_subject \
  FTW_FAKE_FAIL_TARGET=ghcr.io/srcfl/ftw:latest \
  FTW_FAKE_FAIL_OLD_DIGEST="${old_core}"; then
  echo "injected Core failure unexpectedly succeeded" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${old_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${old_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${old_compat_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${old_compat_updater}"
grep -Fq "tag|ghcr.io/srcfl/ftw-updater:latest|${old_updater}" "${log}"

# TERM after the first accepted alias write must audit the still-draft release,
# restore the touched alias, and retain the standard signal status.
write_initial_state
set +e
run_subject \
  FTW_FAKE_SIGNAL_NAME=TERM \
  FTW_FAKE_SIGNAL_TARGET=ghcr.io/srcfl/ftw-updater:latest \
  FTW_FAKE_SIGNAL_DIGEST="${new_updater}" \
  FTW_FAKE_SECOND_SIGNAL_NAME=INT \
  FTW_FAKE_SECOND_SIGNAL_TARGET=ghcr.io/srcfl/ftw-updater:latest \
  FTW_FAKE_SECOND_SIGNAL_DIGEST="${old_updater}"
term_status=$?
set -e
if [ "${term_status}" -ne 143 ]; then
  echo "TERM rollback exited ${term_status}, want 143" >&2
  exit 1
fi
grep -Fq "signal|TERM|ghcr.io/srcfl/ftw-updater:latest" "${log}"
grep -Fq "signal|INT|ghcr.io/srcfl/ftw-updater:latest" "${log}"
grep -Fq "tag|ghcr.io/srcfl/ftw-updater:latest|${old_updater}" "${log}"
if grep -Fq 'release|publish' "${log}"; then
  echo "TERM reached release publication after an alias move" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${old_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${old_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${old_compat_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${old_compat_updater}"

# INT after two accepted writes follows the same safe rollback path and must
# restore both touched canonical aliases without starting publication.
write_initial_state
set +e
run_subject \
  FTW_FAKE_SIGNAL_NAME=INT \
  FTW_FAKE_SIGNAL_TARGET=ghcr.io/srcfl/ftw:latest \
  FTW_FAKE_SIGNAL_DIGEST="${new_core}" \
  FTW_FAKE_SECOND_SIGNAL_NAME=TERM \
  FTW_FAKE_SECOND_SIGNAL_TARGET=ghcr.io/srcfl/ftw:latest \
  FTW_FAKE_SECOND_SIGNAL_DIGEST="${old_core}"
int_status=$?
set -e
if [ "${int_status}" -ne 130 ]; then
  echo "INT rollback exited ${int_status}, want 130" >&2
  exit 1
fi
grep -Fq "signal|INT|ghcr.io/srcfl/ftw:latest" "${log}"
grep -Fq "signal|TERM|ghcr.io/srcfl/ftw:latest" "${log}"
grep -Fq "tag|ghcr.io/srcfl/ftw:latest|${old_core}" "${log}"
grep -Fq "tag|ghcr.io/srcfl/ftw-updater:latest|${old_updater}" "${log}"
if grep -Fq 'release|publish' "${log}"; then
  echo "INT reached release publication after alias moves" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${old_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${old_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${old_compat_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${old_compat_updater}"

# A failure in the compatibility phase must also restore both canonical
# aliases and the compatibility alias that moved before it.
write_initial_state
if run_subject \
  FTW_FAKE_FAIL_TARGET=ghcr.io/frahlg/forty-two-watts:latest \
  FTW_FAKE_FAIL_OLD_DIGEST="${old_compat_core}"; then
  echo "injected compatibility Core failure unexpectedly succeeded" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${old_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${old_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${old_compat_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${old_compat_updater}"
grep -Fq "tag|ghcr.io/srcfl/ftw:latest|${old_core}" "${log}"
grep -Fq "tag|ghcr.io/frahlg/forty-two-watts-updater:latest|${old_compat_updater}" "${log}"

# If any old alias cannot be observed, the script must stop before the first
# tag mutation rather than inventing parity it cannot restore.
write_initial_state
awk -F '|' '$1 != "ghcr.io/frahlg/forty-two-watts-updater:latest"' "${state}" > "${state}.tmp"
mv "${state}.tmp" "${state}"
if run_subject; then
  echo "missing old compatibility alias unexpectedly succeeded" >&2
  exit 1
fi
if grep -q '^tag|' "${log}"; then
  echo "an alias moved despite incomplete rollback state" >&2
  cat "${log}" >&2
  exit 1
fi

# An old digest that exists as a tag value but cannot be read by digest is not
# a safe rollback source. Refuse the run before the first write.
write_initial_state
if run_subject \
  FTW_FAKE_UNREADABLE_REF="ghcr.io/srcfl/ftw-updater@${old_updater}"; then
  echo "unreadable rollback source unexpectedly succeeded" >&2
  exit 1
fi
if grep -q '^tag|' "${log}"; then
  echo "an alias moved despite an unreadable rollback source" >&2
  cat "${log}" >&2
  exit 1
fi

# If a write fails and one touched alias cannot be restored, surface the
# distinct operator-repair status instead of claiming that rollback succeeded.
write_initial_state
set +e
run_subject \
  FTW_FAKE_FAIL_TARGET=ghcr.io/srcfl/ftw:latest \
  FTW_FAKE_FAIL_OLD_DIGEST="${old_core}" \
  FTW_FAKE_FAIL_RESTORE_TARGET=ghcr.io/srcfl/ftw-updater:latest \
  FTW_FAKE_FAIL_RESTORE_DIGEST="${old_updater}"
rollback_status=$?
set -e
if [ "${rollback_status}" -ne 70 ]; then
  echo "incomplete rollback exited ${rollback_status}, want 70" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw-updater:latest "${new_updater}"
assert_digest ghcr.io/srcfl/ftw:latest "${old_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${old_compat_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${old_compat_core}"

# If GitHub confirms that a failed edit left the release as a draft, restore
# all four aliases.
write_initial_state
set +e
run_subject FTW_FAKE_FAIL_BEFORE_EDIT=true
draft_status=$?
set -e
if [ "${draft_status}" -eq 0 ] || [ -f "${final_state}" ]; then
  echo "confirmed draft edit failure did not fail safely" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${old_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${old_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${old_compat_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${old_compat_updater}"

# GitHub may accept the edit and then return an error. If both readbacks prove
# public+latest, keep the new aliases and report success.
write_initial_state
run_subject FTW_FAKE_FAIL_AFTER_EDIT=true
test -f "${final_state}"
assert_digest ghcr.io/srcfl/ftw:latest "${new_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${new_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${new_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${new_updater}"

# An unreadable public view is ambiguous after the edit. Exit with the audit
# code and do not move any alias again.
write_initial_state
set +e
run_subject FTW_FAKE_FAIL_VIEW=true
view_status=$?
set -e
if [ "${view_status}" -ne 71 ] || [ ! -f "${final_state}" ]; then
  echo "ambiguous public view exited ${view_status}, want operator-audit 71" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${new_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${new_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${new_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${new_updater}"

# A public release with an unreadable /releases/latest result is also an audit,
# never a reason to roll back aliases after publication is confirmed.
write_initial_state
set +e
run_subject FTW_FAKE_FAIL_LATEST=true
latest_status=$?
set -e
if [ "${latest_status}" -ne 71 ] || [ ! -f "${final_state}" ]; then
  echo "ambiguous latest read exited ${latest_status}, want operator-audit 71" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${new_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${new_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${new_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${new_updater}"

# A successful edit is already a commit signal. If an immediate read still
# shows the draft, keep the new aliases and require an audit instead of risking
# a delayed public release with rolled-back latest aliases.
write_initial_state
set +e
run_subject FTW_FAKE_SUCCESS_WITH_STALE_DRAFT=true
stale_read_status=$?
set -e
if [ "${stale_read_status}" -ne 71 ] || [ -f "${final_state}" ]; then
  echo "successful edit with stale draft read exited ${stale_read_status}, want audit 71" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${new_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${new_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${new_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${new_updater}"

# A failed edit with a draft view but a latest endpoint that already names the
# candidate is contradictory. Do not guess that rollback is safe.
write_initial_state
set +e
run_subject FTW_FAKE_FAIL_BEFORE_EDIT=true FTW_FAKE_LATEST_TAG=v2.0.0
conflicting_read_status=$?
set -e
if [ "${conflicting_read_status}" -ne 71 ] || [ -f "${final_state}" ]; then
  echo "conflicting draft/latest read exited ${conflicting_read_status}, want audit 71" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${new_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${new_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${new_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${new_updater}"

# Re-running that failed v2.0.0 publish after v2.1.0 became public must stop at
# the fresh monotonic guard, before a tag or release mutation.
cat > "${state}" <<EOF
ghcr.io/srcfl/ftw:latest|${newer_core}
ghcr.io/srcfl/ftw-updater:latest|${newer_updater}
ghcr.io/frahlg/forty-two-watts:latest|${newer_core}
ghcr.io/frahlg/forty-two-watts-updater:latest|${newer_updater}
EOF
: > "${log}"
set +e
run_subject FTW_FAKE_PUBLIC_STABLE=v2.1.0
downgrade_status=$?
set -e
if [ "${downgrade_status}" -eq 0 ]; then
  echo "old failed publish rerun could downgrade newer public stable" >&2
  exit 1
fi
if grep -q '^tag|' "${log}" || grep -Fq 'release|publish' "${log}"; then
  echo "old failed publish rerun mutated aliases or release" >&2
  cat "${log}" >&2
  exit 1
fi
assert_digest ghcr.io/srcfl/ftw:latest "${newer_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${newer_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${newer_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${newer_updater}"

# Existing false parity is not a rollback baseline. Refuse it before writes.
write_initial_state
wrong_compat="sha256:$(printf '9%.0s' {1..64})"
awk -F '|' -v replacement="${wrong_compat}" '
  $1 == "ghcr.io/frahlg/forty-two-watts:latest" { print $1 "|" replacement; next }
  { print }
' "${state}" > "${state}.tmp"
mv "${state}.tmp" "${state}"
if run_subject; then
  echo "mismatched canonical and compatibility aliases unexpectedly succeeded" >&2
  exit 1
fi
if grep -q '^tag|' "${log}"; then
  echo "an alias moved from a false-parity baseline" >&2
  cat "${log}" >&2
  exit 1
fi

# Happy path moves all four aliases to the two paired candidate digests.
write_initial_state
run_subject
assert_digest ghcr.io/srcfl/ftw:latest "${new_core}"
assert_digest ghcr.io/srcfl/ftw-updater:latest "${new_updater}"
assert_digest ghcr.io/frahlg/forty-two-watts:latest "${new_core}"
assert_digest ghcr.io/frahlg/forty-two-watts-updater:latest "${new_updater}"
test -f "${final_state}"

echo "paired latest alias rollback checks passed"
