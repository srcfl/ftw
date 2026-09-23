#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

printf 'existing FTW data\n' > "$work/data"
printf 'existing Compose file\n' > "$work/docker-compose.yml"
bash "$root/scripts/enable-modular-stack.sh" "$work/docker-compose.yml" > "$work/out"
grep -q 'Energyplan ships with Core' "$work/out"
grep -q 'existing Compose file' "$work/docker-compose.yml"
test ! -e "$work/docker-compose.override.yml"

for script in migrate-legacy-compose.sh install-macos.sh; do
  if bash "$root/scripts/$script" --dir "$work" > "$work/out" 2>&1; then
    echo "$script accepted a retired Docker installation path" >&2
    exit 1
  fi
  grep -q 'retired' "$work/out"
  grep -q 'This script makes no changes' "$work/out"
done
grep -q 'existing FTW data' "$work/data"
grep -q 'existing Compose file' "$work/docker-compose.yml"
test ! -e "$work/docker-compose.override.yml"
echo 'retired Docker installer and migration guards passed'
