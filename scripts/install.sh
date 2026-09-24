#!/usr/bin/env bash
# Install one published native 0.x release on a fresh Linux host.
# Existing FTW installations need the separate guided migration.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: install.sh --fresh-host --tag v0.X.Y[-beta.N]
       install.sh --resume --tag v0.X.Y[-beta.N]
       install.sh --refresh --tag v0.X.Y[-beta.N]

Install one exact published native 0.x release on a fresh 64-bit Linux host.
--fresh-host confirms that no FTW site exists on this host, including a
stopped Docker site installed in a custom directory. --resume only continues
an interrupted native install with the same tag and package checksum.
--refresh replaces the launcher, the ftw command and the service definition
of an install this script made with those from the given release. Updates
never replace these files; Core itself moves with ftw update.
There is no default tag: GitHub releases/latest still serves old Docker boxes.
An existing FTW installation must wait for the guided 0.x migration. This
script will not replace it or update a Docker container.
EOF
}

if [[ "${1:-}" == --help || "${1:-}" == -h ]]; then
  usage
  exit 0
fi
if [[ "$#" != 3 || ( "${1:-}" != --fresh-host && "${1:-}" != --resume && "${1:-}" != --refresh ) || "${2:-}" != --tag ]]; then
  usage >&2
  exit 2
fi
mode="$1"
tag="$3"
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

pending=/etc/ftw-native-install.pending
# These checks catch common layouts. --fresh-host also covers a stopped
# Docker site in a custom FTW_DIR that cannot be found from a fixed path.
legacy_paths=(
  "$HOME/ftw/docker-compose.yml"
  "$HOME/forty-two-watts/docker-compose.yml"
)
installed_paths=(/opt/ftw/slots.json /opt/ftw/ftw-launcher /etc/systemd/system/ftw.service)
if [[ "$mode" == --refresh ]]; then
  existing_paths=()
  for path in "${installed_paths[@]}"; do
    if [[ ! -e "$path" ]]; then
      echo "No native install made by this script was found ($path is missing); --refresh changes nothing." >&2
      exit 2
    fi
  done
elif [[ "$mode" == --fresh-host ]]; then
  existing_paths=(/opt/ftw /var/lib/ftw /etc/systemd/system/ftw.service /usr/local/bin/ftw
    /etc/systemd/system/forty-two-watts.service "$pending" "${legacy_paths[@]}")
else
  existing_paths=("${legacy_paths[@]}")
  if [[ ! -f "$pending" ]]; then
    echo "No interrupted native install found at $pending; --resume cannot start a new site." >&2
    exit 2
  fi
fi
for path in "${existing_paths[@]}"; do
  if [[ -e "$path" || -L "$path" ]]; then
    echo "Existing FTW installation found at $path. Keep it; \"Coming from an older FTW\" in docs/native-beta.md shows how to try 0.x beside it." >&2
    exit 2
  fi
done
if [[ "$mode" != --refresh ]] && {
   systemctl is-active --quiet ftw.service >/dev/null 2>&1 ||
   systemctl is-active --quiet forty-two-watts.service >/dev/null 2>&1 ||
   { [[ "$mode" == --fresh-host ]] &&
     { systemctl cat ftw.service >/dev/null 2>&1 ||
       systemctl cat forty-two-watts.service >/dev/null 2>&1; }; }; }; then
  echo "An FTW service already exists; refusing a fresh install." >&2
  exit 2
fi
# Cards made from the old FTW image logged in as ftw, so people pick it again.
if [[ "$mode" == --fresh-host ]] && id ftw >/dev/null 2>&1; then
  echo "A user named ftw already exists. FTW runs as its own ftw account, so install from a login with another name; on a Raspberry Pi, write the card again and choose a different username." >&2
  exit 2
fi
if [[ "$mode" != --refresh ]] && command -v ss >/dev/null 2>&1 &&
   ss -ltnH | awk '$4 ~ /:8080$/ { found = 1 } END { exit !found }'; then
  echo "Port 8080 is already in use; refusing a fresh install." >&2
  exit 2
fi

if (( EUID == 0 )); then
  as_root() { "$@"; }
else
  if ! command -v sudo >/dev/null 2>&1; then
    echo "sudo is required to install the service." >&2
    exit 2
  fi
  as_root() { sudo "$@"; }
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
archive="ftw-linux-${arch}.tar.gz"
url="https://github.com/srcfl/ftw/releases/download/${tag}"
protocols=(--proto '=https' --proto-redir '=https')
# FTW_RELEASE_MIRROR serves /download/<tag>/ as GitHub does. It exists to
# test unpublished releases; leave it unset.
if [[ -n "${FTW_RELEASE_MIRROR:-}" ]]; then
  url="${FTW_RELEASE_MIRROR%/}/download/${tag}"
  protocols=()
  echo "Using the release mirror at ${FTW_RELEASE_MIRROR} (for testing)." >&2
fi
curl --fail --silent --show-error --location --retry 3 "${protocols[@]}" \
  "${url}/${archive}" -o "${work}/${archive}"
curl --fail --silent --show-error --location --retry 3 "${protocols[@]}" \
  "${url}/${archive}.sha256" -o "${work}/${archive}.sha256"

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
if (( EUID != 0 )) && ! sudo -n true 2>/dev/null; then sudo -v; fi
if [[ "$mode" == --refresh ]]; then
  for path in "${installed_paths[@]}"; do
    if ! as_root test -f "$path" || as_root test -L "$path"; then
      echo "The native install at $path is missing or a symlink; --refresh changes nothing." >&2
      exit 2
    fi
  done
  release="${stage}/releases/${tag}"
  # The launcher is not running: it replaced itself with Core when it
  # started it, so its file can be swapped in place.
  as_root install -m 0755 -o ftw -g ftw "${work}/ftw-launcher" /opt/ftw/ftw-launcher
  if [[ -f "${release}/ftw-cli" ]]; then
    as_root install -m 0755 -o root -g root "${release}/ftw-cli" /usr/local/bin/ftw
  fi
  if as_root cmp -s "${release}/deploy/ftw-native.service" /etc/systemd/system/ftw.service; then
    unit=unchanged
  else
    as_root install -m 0644 "${release}/deploy/ftw-native.service" /etc/systemd/system/ftw.service
    as_root systemctl daemon-reload
    unit=changed
  fi
  echo "The launcher and the ftw command now come from $tag. Core itself is unchanged; ftw update moves it."
  if [[ "$unit" == changed ]]; then
    echo "The service definition changed. It applies at the next restart: sudo systemctl restart ftw"
  fi
  exit 0
fi
for path in "${existing_paths[@]}"; do
  if as_root test -e "$path" || as_root test -L "$path"; then
    echo "Existing FTW installation found at $path. Keep it; \"Coming from an older FTW\" in docs/native-beta.md shows how to try 0.x beside it." >&2
    exit 2
  fi
done
if as_root systemctl is-active --quiet ftw.service >/dev/null 2>&1 ||
   as_root systemctl is-active --quiet forty-two-watts.service >/dev/null 2>&1; then
  echo "An FTW service is running; refusing to install." >&2
  exit 2
fi
if [[ "$mode" == --fresh-host ]] &&
   { as_root systemctl cat ftw.service >/dev/null 2>&1 || id ftw >/dev/null 2>&1; }; then
  echo "An FTW service or account appeared during package verification; refusing to install." >&2
  exit 2
fi
if command -v ss >/dev/null 2>&1 &&
   ss -ltnH | awk '$4 ~ /:8080$/ { found = 1 } END { exit !found }'; then
  echo "Port 8080 is now in use; refusing to install." >&2
  exit 2
fi
if [[ "$mode" == --resume ]]; then
  if as_root test -L "$pending"; then
    echo "The pending native install record is a symlink; refusing to resume." >&2
    exit 2
  fi
  if [[ "$(as_root cat "$pending")" != "$tag $expected" ]]; then
    echo "The interrupted install used another tag or checksum; refusing to replace it." >&2
    exit 2
  fi
  if as_root test -L /opt/ftw ||
     { as_root test -e /opt/ftw && ! as_root test -d /opt/ftw; }; then
    echo "The pending native install has an unsafe /opt/ftw path." >&2
    exit 2
  fi
  if as_root test -L /etc/systemd/system/ftw.service; then
    echo "The pending native service is a symlink; refusing to resume." >&2
    exit 2
  fi
  if as_root test -e /etc/systemd/system/ftw.service &&
     ! as_root cmp -s /etc/systemd/system/ftw.service \
       "${stage}/releases/${tag}/deploy/ftw-native.service"; then
    echo "The pending native service differs from the verified package; refusing to replace it." >&2
    exit 2
  fi
  if as_root test -e /etc/systemd/system/forty-two-watts.service ||
     as_root test -L /etc/systemd/system/forty-two-watts.service; then
    alias_target="$(as_root readlink /etc/systemd/system/forty-two-watts.service || true)"
    if [[ "$alias_target" != ftw.service && "$alias_target" != /etc/systemd/system/ftw.service ]]; then
      echo "An unrelated FTW service alias exists; refusing to resume." >&2
      exit 2
    fi
  fi
else
  printf '%s %s\n' "$tag" "$expected" > "${work}/pending"
  as_root install -m 0600 "${work}/pending" "$pending"
fi
if ! id ftw >/dev/null 2>&1; then
  as_root useradd --system --user-group --home-dir /var/lib/ftw --no-create-home ftw
fi
as_root install -d -m 0755 /opt/ftw
as_root cp -a "${stage}/." /opt/ftw/
as_root install -m 0755 "${work}/ftw-launcher" /opt/ftw/ftw-launcher
as_root chown -R ftw:ftw /opt/ftw
as_root /opt/ftw/ftw-launcher -root /opt/ftw status >/dev/null
# The operator command. Root owns this copy, so the service account cannot
# replace a program that people may run with sudo.
operator_command=no
if as_root test -f "/opt/ftw/releases/${tag}/ftw-cli"; then
  as_root install -m 0755 -o root -g root "/opt/ftw/releases/${tag}/ftw-cli" /usr/local/bin/ftw
  operator_command=yes
fi
as_root install -m 0644 \
  "${stage}/releases/${tag}/deploy/ftw-native.service" \
  /etc/systemd/system/ftw.service
as_root systemctl daemon-reload
if ! as_root systemctl enable --now ftw.service; then
  as_root systemctl disable --now ftw.service || true
  echo "FTW did not start. Inspect journalctl -u ftw, then rerun with --resume --tag $tag. Persistent data remains in place." >&2
  exit 1
fi
for _ in 1 2 3; do
  sleep 2
  if ! as_root systemctl is-active --quiet ftw.service; then
    as_root systemctl disable --now ftw.service || true
    echo "FTW stopped during startup. Inspect journalctl -u ftw, then rerun with --resume --tag $tag. Persistent data remains in place." >&2
    exit 1
  fi
done
as_root rm "$pending"

echo "Native FTW $tag is running. Open http://<host>:8080/setup to finish setup."
echo "Check the reported version, storage health and live device readings before use."
if [[ "$operator_command" == yes ]]; then
  echo "On this machine, ftw status shows its state, ftw update installs a newer release"
  echo "and ftw backup makes a verified backup. Run ftw help for the rest."
fi
