#!/bin/bash
# Build release archives and attach them to a gated draft release.
# Usage: ./scripts/release.sh v0.2.0
#
# Produces linux arm64/amd64 tarballs + windows amd64 zip via the
# Makefile's `release` target, then uploads them to the GitHub release
# for the given tag. This script cannot create or publish a release. Only the
# release workflow may publish after it checks the Core/updater pair.

set -euo pipefail

VERSION=${1:?Usage: $0 <version>}
REPO="srcfl/ftw"

echo "Building FTW ${VERSION} (linux arm64/amd64, windows amd64)…"

RELEASE_JSON="$(gh release view "${VERSION}" --repo "${REPO}" --json isDraft,assets)" || {
    echo "Release ${VERSION} does not exist. Create it through release.yml." >&2
    exit 1
}
if [ "$(jq -r .isDraft <<<"${RELEASE_JSON}")" != "true" ]; then
    echo "Release ${VERSION} is not a draft; refusing local changes." >&2
    exit 1
fi
if ! jq -e '.assets | any(.name == "ftw-control-plane.json")' <<<"${RELEASE_JSON}" >/dev/null; then
    echo "Draft ${VERSION} lacks ftw-control-plane.json." >&2
    exit 1
fi

make release

ASSETS=(
    release/ftw-linux-arm64.tar.gz
    release/ftw-linux-arm64.tar.gz.sha256
    release/ftw-linux-amd64.tar.gz
    release/ftw-linux-amd64.tar.gz.sha256
    release/ftw-windows-amd64.zip
    release/ftw-windows-amd64.zip.sha256
    release/forty-two-watts-linux-arm64.tar.gz
    release/forty-two-watts-linux-arm64.tar.gz.sha256
    release/forty-two-watts-linux-amd64.tar.gz
    release/forty-two-watts-linux-amd64.tar.gz.sha256
    release/forty-two-watts-windows-amd64.zip
    release/forty-two-watts-windows-amd64.zip.sha256
)

for f in "${ASSETS[@]}"; do
    [ -f "$f" ] || { echo "missing: $f"; exit 1; }
done

echo "Uploading assets to verified draft ${VERSION}…"
gh release upload "${VERSION}" --repo "${REPO}" --clobber "${ASSETS[@]}"

echo "Assets attached. Only release-assets.yml may publish the draft."
