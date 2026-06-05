#!/bin/bash
# Archived legacy deploy helper.
#
# Archived on 2026-06-05. This script targets the old ~/home-ems-app layout
# and preserves state.redb. Current deploy paths use scripts/install.sh,
# scripts/deploy-go.sh, docker-compose, or the GitHub release workflow.
#
# Deploy latest release to a remote host
# Usage: ./docs/archive/scripts/deploy-legacy-home-ems-app.sh homelab-rpi [version]

set -euo pipefail

HOST=${1:?Usage: $0 <ssh-host> [version]}
VERSION=${2:-latest}
REPO="frahlg/forty-two-watts"
# NOTE: live deployments live in ~/home-ems-app (legacy name from before rename).
# Keep using this path so existing config.yaml + state.redb are preserved across deploys.
REMOTE_DIR="home-ems-app"
# Inside the tarball the binary is named with the linux-arm64/amd64 suffix; rename to a stable name.
STABLE_BINARY="forty-two-watts"

# Detect architecture
ARCH=$(ssh ${HOST} "uname -m")
case ${ARCH} in
    aarch64) BINARY="forty-two-watts-linux-arm64" ;;
    x86_64)  BINARY="forty-two-watts-linux-amd64" ;;
    *) echo "Unsupported arch: ${ARCH}"; exit 1 ;;
esac

# Get download URL
if [ "${VERSION}" = "latest" ]; then
    URL=$(gh release view --repo ${REPO} --json assets --jq ".assets[] | select(.name | contains(\"${BINARY}\")) | .url")
else
    URL=$(gh release view ${VERSION} --repo ${REPO} --json assets --jq ".assets[] | select(.name | contains(\"${BINARY}\")) | .url")
fi

echo "Deploying ${BINARY} to ${HOST}..."

ssh ${HOST} "
    set -e
    mkdir -p ~/${REMOTE_DIR}
    cd ~/${REMOTE_DIR}

    # Refuse to nuke an existing config — it's the source of truth
    if [ ! -f config.yaml ]; then
        echo 'No config.yaml found in ~/${REMOTE_DIR} — copy/create it before deploying.'
        exit 1
    fi

    # Stop running instance
    pkill -f ${STABLE_BINARY} 2>/dev/null || true
    sleep 1

    # Download into a staging dir and copy in only what we want — preserves config.yaml + state.redb
    rm -rf .deploy-staging
    mkdir .deploy-staging
    curl -sL '${URL}' | tar xz -C .deploy-staging

    # Backup current binary
    [ -f ${STABLE_BINARY} ] && cp ${STABLE_BINARY} ${STABLE_BINARY}.bak

    # Replace binary, web/, drivers/ — leave config.yaml + state.redb alone
    cp .deploy-staging/${BINARY} ${STABLE_BINARY}
    chmod +x ${STABLE_BINARY}
    rm -rf web drivers
    cp -r .deploy-staging/web web
    cp -r .deploy-staging/drivers drivers
    rm -rf .deploy-staging

    echo 'Binary updated to ${VERSION}'

    # Start
    nohup ./${STABLE_BINARY} config.yaml > forty-two-watts.log 2>&1 &
    sleep 3

    # Verify
    if curl -sf http://localhost:8080/api/health > /dev/null; then
        echo 'Deployed and running!'
        curl -s http://localhost:8080/api/health
        echo
    else
        echo 'WARNING: health check failed'
        tail -10 forty-two-watts.log
    fi
"
