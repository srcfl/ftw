#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

printf 'existing FTW data\n' > "$work/data"
if bash "$root/scripts/upgrade-paired-release.sh" --tag v3.8.0 --dir "$work" > "$work/out" 2>&1; then
  echo 'retired 2.x-to-3.x upgrade ran' >&2
  exit 1
fi
grep -q '2.x-to-3.x Docker upgrade is retired' "$work/out"
grep -q 'This script makes no changes' "$work/out"
grep -q 'existing FTW data' "$work/data"
echo 'retired paired-upgrade guard passed'
