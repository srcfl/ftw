#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
installer="${root}/scripts/install.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

bash -n "$installer"
bash "$installer" --help > "$work/help"
grep -q 'exact published native 0.x release' "$work/help"

# A power cut must not lose the pending record or the copied release: each is
# flushed before the step that depends on it.
line_of() { grep -nF -- "$1" "$installer" | head -1 | cut -d: -f1; }
synced_between() {
  awk -v a="$1" -v b="$2" 'NR > a && NR < b && $1 == "as_root" && $2 == "sync" { found = 1 } END { exit !found }' "$installer"
}
pending_line="$(line_of 'install -m 0600 "${work}/pending" "$pending"')"
copy_line="$(line_of 'as_root cp -a "${stage}/." /opt/ftw/')"
enable_line="$(line_of 'systemctl enable --now ftw.service')"
if ! synced_between "$pending_line" "$copy_line" || ! synced_between "$copy_line" "$enable_line"; then
  echo "installer starts FTW before its pending record and release are on disk" >&2
  exit 1
fi

if bash "$installer" > "$work/out" 2>&1; then
  echo "installer accepted a missing tag" >&2
  exit 1
fi
if bash "$installer" --tag v0.131.0-beta.1 > "$work/out" 2>&1; then
  echo "installer accepted a fresh install without acknowledgement" >&2
  exit 1
fi
grep -q -- '--fresh-host' "$work/out"

for args in '--tag v0.130.4' '--tag v3.8.0' '--tag v0.131.0-beta.0' \
            '--tag v0.131.0;touch /tmp/ftw-unexpected-install'; do
  read -r -a words <<< "$args"
  if bash "$installer" --fresh-host "${words[@]}" > "$work/out" 2>&1; then
    echo "installer accepted invalid arguments: $args" >&2
    exit 1
  fi
done

if [[ "$(uname -s)" == Linux ]]; then
  if bash "$installer" --resume --tag v0.131.0-beta.1 > "$work/out" 2>&1; then
    echo "installer resumed without a pending install" >&2
    exit 1
  fi
  grep -q 'No interrupted native install found' "$work/out"
  mkdir -p "$work/home/ftw"
  ln -s /no-such-compose "$work/home/ftw/docker-compose.yml"
  if HOME="$work/home" bash "$installer" --fresh-host --tag v0.131.0-beta.1 > "$work/out" 2>&1; then
    echo "installer accepted an existing Compose path" >&2
    exit 1
  fi
  grep -q 'Existing FTW installation found' "$work/out"
  if grep -Eq 'sudo|download|checksum' "$work/out"; then
    echo "installer did not stop before privilege or download" >&2
    exit 1
  fi
fi

if [[ "$(uname -s)" == Linux && ! -e /opt/ftw/slots.json ]]; then
  if bash "$installer" --refresh --tag v0.131.0-beta.1 > "$work/out" 2>&1; then
    echo "installer refreshed a host without a native install" >&2
    exit 1
  fi
  grep -q 'No native install made by this script was found' "$work/out"
  if grep -Eq 'sudo|download|checksum' "$work/out"; then
    echo "refresh did not stop before privilege or download" >&2
    exit 1
  fi
fi

echo 'native fresh-installer guards passed'
