#!/usr/bin/env bash
# Keep the scheduled pin check from comparing device-drivers main with an
# empty, freshly checked-out drivers/ directory.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/bundled-driver-pin.yml"

line_for() {
  local needle="$1"
  awk -v needle="$needle" 'index($0, needle) { print NR; exit }' "$WORKFLOW"
}

fetch_line="$(line_for 'run: make drivers')"
compare_line="$(line_for 'bash scripts/sync-bundled-drivers.sh --behind')"

if [ -z "$fetch_line" ] || [ -z "$compare_line" ]; then
  echo "bundled driver pin workflow is missing its snapshot fetch or comparison" >&2
  exit 1
fi

if [ "$fetch_line" -ge "$compare_line" ]; then
  echo "bundled driver pin workflow compares before fetching the pinned snapshot" >&2
  exit 1
fi

echo "bundled driver pin order is valid: fetch line ${fetch_line}, compare line ${compare_line}"
