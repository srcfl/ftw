#!/bin/bash
# Build release archives and create (or attach to) a GitHub release.
# Usage: ./scripts/release.sh v0.2.0
#
# Produces linux arm64/amd64 tarballs + windows amd64 zip via the
# Makefile's `release` target, then uploads them to the GitHub release
# for the given tag. If the release doesn't exist yet it's created with
# auto-generated notes; if it does exist the assets are added with
# --clobber so a re-run safely replaces a broken archive.

set -euo pipefail

VERSION=${1:?Usage: $0 <version>}
REPO="frahlg/forty-two-watts"

echo "Building forty-two-watts ${VERSION} (linux arm64/amd64, windows amd64)…"
make release relay-web

ASSETS=(
    release/forty-two-watts-linux-arm64.tar.gz
    release/forty-two-watts-linux-amd64.tar.gz
    release/forty-two-watts-windows-amd64.zip
    release/ftw-relay-web.tar.gz
)

for f in "${ASSETS[@]}"; do
    [ -f "$f" ] || { echo "missing: $f"; exit 1; }
done

if gh release view "${VERSION}" --repo "${REPO}" >/dev/null 2>&1; then
    echo "Release ${VERSION} exists — uploading assets (clobbering duplicates)…"
    gh release upload "${VERSION}" --repo "${REPO}" --clobber "${ASSETS[@]}"
else
    echo "Creating GitHub release ${VERSION}…"
    gh release create "${VERSION}" --repo "${REPO}" \
        --title "${VERSION}" --generate-notes \
        "${ASSETS[@]}"
fi

echo "Done! Release: https://github.com/${REPO}/releases/tag/${VERSION}"
