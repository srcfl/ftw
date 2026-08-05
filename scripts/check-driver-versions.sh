#!/usr/bin/env bash
# Require a SemVer bump from any bundled driver whose bytes changed.
#
# The drivers here are generated from srcfl/device-drivers at the commit
# pinned in drivers/BUNDLED_SOURCE.json, so what reaches a gateway is decided
# by where that pin points -- not by what this repository's history shows.
# This compares the drivers at the previous pin against the drivers at the
# current one and fails when a file changed without its DRIVER version moving.
#
# Asking the pins rather than the Git log keeps working once the .lua files
# stop being committed here, and is the more honest question even now: it
# measures what ships.
#
# Usage:
#   scripts/check-driver-versions.sh <base-git-revision>
#
# The base revision is only used to read the previous drivers/BUNDLED_SOURCE.json.
# A pull request that does not move the pin has nothing to check.

set -euo pipefail

BASE="${1:-}"
[ -n "$BASE" ] || { echo "usage: $0 <base-git-revision>" >&2; exit 2; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PIN="drivers/BUNDLED_SOURCE.json"

command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

NEW_PIN_JSON="$(cat "${ROOT}/${PIN}")"
# A pull request that adds the pin for the first time has no previous one.
OLD_PIN_JSON="$(git -C "$ROOT" show "${BASE}:${PIN}" 2>/dev/null || echo '')"

if [ -z "$OLD_PIN_JSON" ]; then
  echo "no ${PIN} at ${BASE}; nothing to compare"
  exit 0
fi

REPOSITORY="$(printf '%s' "$NEW_PIN_JSON" | jq -r .repository)"
SOURCE_DIR="$(printf '%s' "$NEW_PIN_JSON" | jq -r .source_dir)"
NEW_COMMIT="$(printf '%s' "$NEW_PIN_JSON" | jq -r .commit)"
OLD_COMMIT="$(printf '%s' "$OLD_PIN_JSON" | jq -r .commit)"

if [ "$OLD_COMMIT" = "$NEW_COMMIT" ]; then
  echo "pin unchanged at ${NEW_COMMIT:0:12}; no driver bytes can have moved"
  exit 0
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# Materialise one commit's drivers into a flat directory, limited to the
# drivers the current pin actually bundles. A driver added to the list has no
# counterpart in the old snapshot, which reads as "newly bundled".
materialise() {
  local commit="$1" dest="$2"
  mkdir -p "$dest"
  local extract="${WORK}/extract-${commit:0:12}"
  mkdir -p "$extract"
  curl -fsSL "https://codeload.github.com/${REPOSITORY}/tar.gz/${commit}" \
    | tar -xz -C "$extract"
  local src
  src="$(find "$extract" -maxdepth 1 -mindepth 1 -type d | head -1)/${SOURCE_DIR}"
  [ -d "$src" ] || { echo "no ${SOURCE_DIR} at ${commit}" >&2; exit 1; }
  while IFS= read -r id; do
    [ -n "$id" ] || continue
    [ -f "${src}/${id}.lua" ] && cp "${src}/${id}.lua" "${dest}/${id}.lua"
  done < <(printf '%s' "$NEW_PIN_JSON" | jq -r '.drivers[]')
}

echo "comparing ${REPOSITORY} ${OLD_COMMIT:0:12} -> ${NEW_COMMIT:0:12}"
materialise "$OLD_COMMIT" "${WORK}/old"
materialise "$NEW_COMMIT" "${WORK}/new"

cd "${ROOT}/go"
go run ./cmd/ftw-driver-repository check-versions \
  -old-dir "${WORK}/old" -new-dir "${WORK}/new"
