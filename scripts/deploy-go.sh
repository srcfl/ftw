#!/usr/bin/env bash
# Deploy FTW to a remote host — ships the Go binary + Lua
# drivers + web assets.
#
# Usage:
#   ./scripts/deploy-go.sh homelab-rpi [version]
#
# Defaults to "latest" on the GitHub releases. Preserves config.yaml and
# state.db on the remote host.
set -euo pipefail

HOST=${1:?Usage: $0 <ssh-host> [version]}
VERSION=${2:-latest}
REPO="srcfl/ftw"
REMOTE_DIR="${FTW_REMOTE_DIR:-ftw-go}"

# Detect arch
ARCH=$(ssh "$HOST" "uname -m")
case "$ARCH" in
    aarch64) ARCHIVE="ftw-linux-arm64.tar.gz" ;;
    x86_64)  ARCHIVE="ftw-linux-amd64.tar.gz" ;;
    *) echo "Unsupported arch: $ARCH"; exit 1 ;;
esac

# Resolve download URL
if [ "$VERSION" = "latest" ]; then
    URL=$(gh release view --repo "$REPO" --json assets \
        --jq ".assets[] | select(.name == \"${ARCHIVE}\") | .url")
else
    URL=$(gh release view "$VERSION" --repo "$REPO" --json assets \
        --jq ".assets[] | select(.name == \"${ARCHIVE}\") | .url")
fi

echo "Deploying $ARCHIVE @ $VERSION to $HOST..."
ssh "$HOST" "
    set -e
    mkdir -p ~/$REMOTE_DIR
    cd ~/$REMOTE_DIR

    # Refuse to nuke an existing config — operator must seed it intentionally.
    if [ ! -f config.yaml ]; then
        echo 'No config.yaml found in ~/$REMOTE_DIR — copy/create it before deploying.'
        echo 'See config.example.yaml in the tarball for a template.'
        exit 1
    fi

    # Stop running instance
    pkill -f '$REMOTE_DIR/ftw' 2>/dev/null || true
    pkill -f '$REMOTE_DIR/forty-two-watts' 2>/dev/null || true
    sleep 1

    # Stage into temp dir, then atomically swap
    rm -rf .deploy-staging
    mkdir .deploy-staging
    curl -sL '$URL' | tar xz -C .deploy-staging

    # Backup current binary
    [ -f ftw ] && cp ftw ftw.bak

    # Swap binary + web + drivers (config.yaml + *.db untouched)
    cp .deploy-staging/ftw ftw
    ln -sf ftw forty-two-watts
    chmod +x ftw
    rm -rf web drivers
    cp -r .deploy-staging/web web
    cp -r .deploy-staging/drivers drivers
    rm -rf .deploy-staging

    echo 'Binary updated to $VERSION'

    # Start
    nohup ./ftw -config config.yaml -web web > ftw.log 2>&1 &
    sleep 3

    if curl -sf http://localhost:8080/api/health > /dev/null; then
        echo 'Deployed and running!'
        curl -s http://localhost:8080/api/health
        echo
    else
        echo 'WARNING: health check failed'
        tail -10 ftw.log
    fi
"
