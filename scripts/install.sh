#!/usr/bin/env bash
# Install one published native 0.x release on a fresh Linux host.
# Existing FTW installations need the separate guided migration.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: install.sh --tag v0.X.Y[-beta.N]

Install one exact published native 0.x release on a fresh 64-bit Linux host.
There is no default tag: GitHub releases/latest still serves old Docker boxes.
An existing FTW installation must wait for the guided 0.x migration. This
script will not replace it or update a Docker container.
EOF
}

if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
  usage
  exit 0
fi
if [[ "$#" != 2 || "${1:-}" != --tag ]]; then
  usage >&2
  exit 2
fi
tag="$2"
if [[ ! "$tag" =~ ^v0\.([1-9][0-9]*)\.(0|[1-9][0-9]*)(-beta\.([1-9][0-9]*))?$ ]] ||
   (( ${BASH_REMATCH[1]:-0} < 131 )); then
  echo "A native release tag v0.131.0 or later is required: $tag" >&2
  exit 2
fi

if [[ "$(uname -s)" != Linux ]]; then
  echo "The native installer requires Linux and systemd." >&2
  exit 2
fi
case "$(uname -m)" in
  aarch64|arm64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) echo "A 64-bit ARM or AMD64 host is required." >&2; exit 2 ;;
esac
for tool in curl tar sha256sum mktemp systemctl; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "Missing required tool: $tool" >&2
    exit 2
  fi
done

# Refuse a known running site before asking for sudo. The root check below
# repeats the path check in case a directory was hidden from this account.
existing_paths=(
  /opt/ftw /var/lib/ftw /etc/systemd/system/ftw.service
  /etc/systemd/system/forty-two-watts.service
  "$HOME/ftw/docker-compose.yml"
  "$HOME/forty-two-watts/docker-compose.yml"
)
for path in "${existing_paths[@]}"; do
  if [[ -e "$path" || -L "$path" ]]; then
    echo "Existing FTW installation found at $path; leave it running and use the guided 0.x migration when available." >&2
    exit 2
  fi
done
if systemctl is-active --quiet ftw.service >/dev/null 2>&1 ||
   systemctl is-active --quiet forty-two-watts.service >/dev/null 2>&1 ||
   id ftw >/dev/null 2>&1; then
  echo "An FTW service or account already exists; refusing a fresh install." >&2
  exit 2
fi

if (( EUID == 0 )); then
  as_root() { "$@"; }
else
  if ! command -v sudo >/dev/null 2>&1; then
    echo "sudo is required to install the service." >&2
    exit 2
  fi
  sudo -v
  as_root() { sudo "$@"; }
fi

# A fresh installer must never take ownership of a site that is already
# controlling equipment. Check known native paths, old Compose homes and both
# service names before downloading or changing any host state.
for path in "${existing_paths[@]}"; do
  if as_root test -e "$path" || as_root test -L "$path"; then
    echo "Existing FTW installation found at $path; leave it running and use the guided 0.x migration when available." >&2
    exit 2
  fi
done
if as_root systemctl is-active --quiet ftw.service >/dev/null 2>&1 ||
   as_root systemctl is-active --quiet forty-two-watts.service >/dev/null 2>&1 ||
   id ftw >/dev/null 2>&1; then
  echo "An FTW service or account already exists; refusing a fresh install." >&2
  exit 2
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
archive="ftw-linux-${arch}.tar.gz"
url="https://github.com/srcfl/ftw/releases/download/${tag}"
curl --fail --silent --show-error --location --retry 3 --proto '=https' \
  --proto-redir '=https' "${url}/${archive}" -o "${work}/${archive}"
curl --fail --silent --show-error --location --retry 3 --proto '=https' \
  --proto-redir '=https' "${url}/${archive}.sha256" -o "${work}/${archive}.sha256"

checksum="$(cat "${work}/${archive}.sha256")"
expected="${checksum:0:64}"
if [[ ! "$expected" =~ ^[0-9a-f]{64}$ ||
      "${checksum:64:2}" != "  " || "${checksum:66}" != "$archive" ]]; then
  echo "The release checksum does not name the expected package." >&2
  exit 1
fi
actual="$(sha256sum "${work}/${archive}" | cut -d ' ' -f 1)"
if [[ "$actual" != "$expected" ]]; then
  echo "The release package checksum does not match." >&2
  exit 1
fi

# The package's launcher checks every archive entry, the embedded version,
# architecture and state schema before it makes a complete release visible.
tar -xOf "${work}/${archive}" ftw-launcher > "${work}/ftw-launcher"
chmod 0755 "${work}/ftw-launcher"
stage="${work}/slot-root"
mkdir -p "$stage"
"${work}/ftw-launcher" -root "$stage" install "$tag" \
  "${work}/${archive}" "${work}/${archive}.sha256"
"${work}/ftw-launcher" -root "$stage" init "$tag"
"${work}/ftw-launcher" -root "$stage" status >/dev/null

# No host write occurs until the whole package has passed verification.
as_root useradd --system --user-group --home-dir /var/lib/ftw --no-create-home ftw
as_root install -d -m 0755 /opt/ftw
as_root cp -a "${stage}/." /opt/ftw/
as_root install -m 0755 "${work}/ftw-launcher" /opt/ftw/ftw-launcher
as_root chown -R ftw:ftw /opt/ftw
as_root install -m 0644 \
  "${stage}/releases/${tag}/deploy/ftw-native.service" \
  /etc/systemd/system/ftw.service
as_root systemctl daemon-reload
if ! as_root systemctl enable --now ftw.service; then
  as_root systemctl disable --now ftw.service || true
  echo "FTW did not start; the verified package remains at /opt/ftw for inspection." >&2
  exit 1
fi
for _ in 1 2 3; do
  sleep 2
  if ! as_root systemctl is-active --quiet ftw.service; then
    as_root systemctl disable --now ftw.service || true
    echo "FTW stopped during startup; inspect journalctl -u ftw before retrying." >&2
    exit 1
  fi
done

echo "Native FTW $tag is running. Open http://<host>:8080/setup to finish setup."
echo "Check the reported version, storage health and live device readings before use."
