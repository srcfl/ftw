#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
installer="${root}/scripts/install.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

bash -n "$installer"
bash "$installer" --help > "$work/help"
grep -q 'exact published native 0.x release' "$work/help"

if bash "$installer" > "$work/out" 2>&1; then
  echo "installer accepted a missing tag" >&2
  exit 1
fi

for args in '--tag v0.130.4' '--tag v3.8.0' '--tag v0.131.0-beta.0' \
            '--tag v0.131.0;touch /tmp/ftw-unexpected-install'; do
  read -r -a words <<< "$args"
  if bash "$installer" "${words[@]}" > "$work/out" 2>&1; then
    echo "installer accepted invalid arguments: $args" >&2
    exit 1
  fi
done

if [[ "$(uname -s)" == Linux ]]; then
  mkdir -p "$work/home/ftw"
  ln -s /no-such-compose "$work/home/ftw/docker-compose.yml"
  if HOME="$work/home" bash "$installer" --tag v0.131.0-beta.1 > "$work/out" 2>&1; then
    echo "installer accepted an existing Compose path" >&2
    exit 1
  fi
  grep -q 'Existing FTW installation found' "$work/out"
  if grep -Eq 'sudo|download|checksum' "$work/out"; then
    echo "installer did not stop before privilege or download" >&2
    exit 1
  fi
fi

echo 'native fresh-installer guards passed'
