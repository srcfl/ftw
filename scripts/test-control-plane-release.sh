#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

mkdir -p "${TMP}/bin"
cat >"${TMP}/bin/docker" <<'FAKE_DOCKER'
#!/usr/bin/env bash
set -euo pipefail
ref="${*: -1}"
case "${ref}" in
  ghcr.io/srcfl/ftw:v1.4.0)
    echo 'Name: test'
    echo 'Digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    ;;
  ghcr.io/srcfl/ftw-updater:v1.4.0)
    echo 'Name: test'
    echo "Digest: ${FAKE_UPDATER_DIGEST:-sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb}"
    ;;
  *) exit 1 ;;
esac
FAKE_DOCKER
chmod +x "${TMP}/bin/docker"

cd "${ROOT}"
PATH="${TMP}/bin:${PATH}" scripts/create-control-plane-manifest.sh \
  v1.4.0 0123456789abcdef0123456789abcdef01234567 "${TMP}/manifest.json"
PATH="${TMP}/bin:${PATH}" scripts/verify-control-plane-manifest.sh \
  "${TMP}/manifest.json" v1.4.0 0123456789abcdef0123456789abcdef01234567

if PATH="${TMP}/bin:${PATH}" FAKE_UPDATER_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
  scripts/verify-control-plane-manifest.sh "${TMP}/manifest.json" v1.4.0 \
  0123456789abcdef0123456789abcdef01234567 >/dev/null 2>&1; then
  echo "moved updater tag should fail pair verification" >&2
  exit 1
fi

if PATH="${TMP}/bin:${PATH}" scripts/verify-control-plane-manifest.sh \
  "${TMP}/manifest.json" v1.4.1 0123456789abcdef0123456789abcdef01234567 >/dev/null 2>&1; then
  echo "wrong release identity should fail pair verification" >&2
  exit 1
fi

echo "control-plane release contract verified"
