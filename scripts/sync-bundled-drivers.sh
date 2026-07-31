#!/usr/bin/env bash
# Regenerate drivers/ from the device-drivers commit pinned in
# drivers/BUNDLED_SOURCE.json.
#
# The bundled drivers are a recovery snapshot, not a source. Startup is
# deliberately local -- a gateway boots and runs offline, and a remote refresh
# must never block it -- so the files have to be in the image. But keeping them
# by hand made this a second source of truth, and it drifted: a fix that landed
# in device-drivers left the bundled copy behind, still carrying the bug.
#
# Usage:
#   scripts/sync-bundled-drivers.sh            # write the snapshot
#   scripts/sync-bundled-drivers.sh --check    # fail if any driver is committed
#   scripts/sync-bundled-drivers.sh --behind   # fail if the pin is out of date
#
# To take a newer driver: move the commit in BUNDLED_SOURCE.json, run this, and
# commit the result. To add or drop a bundled driver, edit the list there --
# which drivers are bundled is FTW's decision, and device-drivers publishes
# more than FTW needs to carry for recovery.
#
# --check and --behind ask opposite questions. --check catches a driver being
# committed here at all, and runs on every pull request. --behind catches the
# pin being left behind while a fix lands upstream, and runs on a schedule --
# nothing noticed that until a Pixii flap reached customer hardware a second
# time.
#
# --check used to compare committed bytes against the pin. There are no
# committed bytes now: the drivers are gitignored and fetched, so the only way
# to make this a second source again is to commit one, and that is the single
# thing --check has left to look for. Cheaper than a content diff, and a
# stricter rule.
#
# --behind compares against the drivers upstream, not against the commit id, so
# a doc change or a dependency bump in device-drivers stays quiet. It only
# speaks up when a bundled driver would actually change.

set -euo pipefail

CHECK=0
BEHIND=0
case "${1:-}" in
  --check)  CHECK=1 ;;
  --behind) BEHIND=1; CHECK=1 ;;
  "")       ;;
  *) echo "unknown option '${1}'; see the header for usage" >&2; exit 2 ;;
esac

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIN="${ROOT}/drivers/BUNDLED_SOURCE.json"
DEST="${ROOT}/drivers"

command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
[ -f "$PIN" ] || { echo "missing ${PIN}" >&2; exit 2; }

# --check is a question about this repository, not about upstream, so it
# answers before any network call. The drivers are gitignored and fetched;
# committing one is the only way to make this a second source of truth again.
if [ "$CHECK" = "1" ] && [ "$BEHIND" = "0" ]; then
  committed="$(git -C "$ROOT" ls-files -- 'drivers/*.lua')"
  if [ -n "$committed" ]; then
    echo "$committed" | sed 's/^/COMMITTED /' >&2
    cat >&2 <<'MSG'

A driver is committed under drivers/.

These files are fetched from srcfl/device-drivers at the commit pinned in
drivers/BUNDLED_SOURCE.json, and they are gitignored so that stays true. A
driver committed here is a second source of truth, which is how a Sungrow fix
once landed upstream while the bundled copy kept the bug that took a
customer's inverter offline.

Fix the driver in srcfl/device-drivers, move the pin, and run
scripts/sync-bundled-drivers.sh.
MSG
    exit 1
  fi
  echo "no driver source is committed here (drivers/ is fetched from the pin)"
  exit 0
fi

REPOSITORY="$(jq -r .repository "$PIN")"
COMMIT="$(jq -r .commit "$PIN")"
SOURCE_DIR="$(jq -r .source_dir "$PIN")"
PINNED="$COMMIT"

# The branch to measure against. device-drivers releases from main.
UPSTREAM_BRANCH="${UPSTREAM_BRANCH:-main}"

if [ "$BEHIND" = "1" ]; then
  COMMIT="$(curl -fsSL \
    -H "Accept: application/vnd.github+json" \
    ${GITHUB_TOKEN:+-H "Authorization: Bearer ${GITHUB_TOKEN}"} \
    "https://api.github.com/repos/${REPOSITORY}/commits/${UPSTREAM_BRANCH}" \
    | jq -r .sha)"
  [ -n "$COMMIT" ] && [ "$COMMIT" != "null" ] \
    || { echo "could not resolve ${REPOSITORY}@${UPSTREAM_BRANCH}" >&2; exit 2; }
fi
# Read into an array the long way: the bash that ships with macOS has no
# mapfile, and this script has to run the same locally and in CI.
DRIVERS=()
while IFS= read -r line; do
  [ -n "$line" ] && DRIVERS+=("$line")
done < <(jq -r '.drivers[]' "$PIN")

case "$COMMIT" in
  [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
  *) echo "commit must be a full hex sha, got '${COMMIT}'" >&2; exit 2 ;;
esac

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

echo "fetching ${REPOSITORY} at ${COMMIT:0:12}"
# A tarball of one commit rather than a clone: no history, no refs, and the
# commit cannot move under us the way a branch name could.
curl -fsSL "https://codeload.github.com/${REPOSITORY}/tar.gz/${COMMIT}" \
  | tar -xz -C "${WORK}"

SRC="$(find "${WORK}" -maxdepth 1 -mindepth 1 -type d | head -1)/${SOURCE_DIR}"
[ -d "$SRC" ] || { echo "no ${SOURCE_DIR} at that commit" >&2; exit 1; }

drift=0
missing=0
for id in "${DRIVERS[@]}"; do
  from="${SRC}/${id}.lua"
  to="${DEST}/${id}.lua"
  if [ ! -f "$from" ]; then
    echo "MISSING upstream: ${id}.lua is not in ${REPOSITORY} at this commit" >&2
    missing=1
    continue
  fi
  if [ "$CHECK" = "1" ]; then
    if ! cmp -s "$from" "$to"; then
      if [ "$BEHIND" = "1" ]; then
        echo "OUTDATED ${id}.lua"
      else
        echo "DRIFTED ${id}.lua"
      fi
      drift=1
    fi
  else
    cp "$from" "$to"
  fi
done

# A .lua in drivers/ that the pin does not list is a file nothing regenerates,
# which is how the second source of truth grew in the first place. Skipped in
# --behind mode, which asks about the pin rather than about local files.
if [ "$BEHIND" = "0" ]; then
  for path in "${DEST}"/*.lua; do
    [ -e "$path" ] || continue
    id="$(basename "$path" .lua)"
    listed=0
    for known in "${DRIVERS[@]}"; do
      [ "$id" = "$known" ] && { listed=1; break; }
    done
    if [ "$listed" = "0" ]; then
      echo "UNLISTED ${id}.lua is in drivers/ but not in BUNDLED_SOURCE.json" >&2
      drift=1
    fi
  done
fi

if [ "$missing" = "1" ]; then
  echo "the pin names drivers that do not exist upstream" >&2
  exit 1
fi

if [ "$BEHIND" = "1" ]; then
  if [ "$drift" = "1" ]; then
    cat >&2 <<MSG

The bundled driver pin is out of date.

  pinned:   ${PINNED}
  upstream: ${COMMIT}  (${REPOSITORY}@${UPSTREAM_BRANCH})

The drivers listed above have changed upstream. A gateway that boots offline
runs the bundled copy, so until the pin moves it keeps running the old one.

  1. set .commit in drivers/BUNDLED_SOURCE.json to ${COMMIT}
  2. run scripts/sync-bundled-drivers.sh
  3. commit the result

Read what changed before taking it -- moving the pin takes every driver at
that commit, not only the ones you came for.
MSG
    exit 1
  fi
  if [ "$PINNED" = "$COMMIT" ]; then
    echo "pin is current: ${REPOSITORY}@${COMMIT:0:12}"
  else
    echo "pin is ${PINNED:0:12}, ${UPSTREAM_BRANCH} is ${COMMIT:0:12}, and no bundled driver differs"
  fi
elif [ "$CHECK" = "1" ]; then
  if [ "$drift" = "1" ]; then
    cat >&2 <<'MSG'

drivers/ no longer matches the pinned device-drivers commit.

These files are generated. Fix the driver in srcfl/device-drivers, move the
commit in drivers/BUNDLED_SOURCE.json, run scripts/sync-bundled-drivers.sh and
commit the result. Editing them here makes this a second source, and the copies
then drift apart silently.
MSG
    exit 1
  fi
  echo "drivers/ matches ${REPOSITORY}@${COMMIT:0:12} (${#DRIVERS[@]} drivers)"
else
  echo "wrote ${#DRIVERS[@]} drivers from ${REPOSITORY}@${COMMIT:0:12}"
fi
