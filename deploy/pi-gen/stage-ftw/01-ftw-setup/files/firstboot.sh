#!/bin/bash
# First-boot provisioner for FTW.
#
# Runs once under ftw-firstboot.service. Pulls the docker-compose
# stack's images from GHCR and brings the services up. Idempotent —
# re-running after the sentinel lands is a no-op, and a failed run
# leaves the sentinel untouched so the next boot retries.

set -euo pipefail

SENTINEL=/var/lib/ftw/firstboot.done
LOG=/var/log/ftw-firstboot.log

mkdir -p "$(dirname "${SENTINEL}")"

# Tee all output so the log file has a durable record even if
# journald's ring rotates. systemd still captures stdout via the
# service unit, so journalctl -u ftw-firstboot also works.
exec > >(tee -a "${LOG}") 2>&1
echo "[$(date -Is)] ftw-firstboot starting"

cd /opt/ftw

# The image build parks Docker's apt source so pi-gen's export-image apt
# step cannot OOM on it (see 01-ftw-setup/00-run.sh). Restore it on the
# real device: without it the engine is frozen at the image's build
# version for the appliance's whole service life (srcfl/ftw#770). The
# signing key at /etc/apt/keyrings/docker.asc was never removed.
if [ -f /etc/apt/sources.list.d/docker.list.disabled ]; then
    mv /etc/apt/sources.list.d/docker.list.disabled /etc/apt/sources.list.d/docker.list
fi

# Bring the stack up on whatever images are already present FIRST, so a box
# that already has them (a reboot mid-provision, a re-run, or a future
# pre-baked image) is never held hostage by GHCR: GHCR is GitHub-hosted, so a
# GitHub outage makes `compose pull` fail. `docker compose up -d` only reaches
# out to GHCR for an image that is genuinely missing locally.
#
# A truly fresh box has no local images, so `up -d` fails; it then retries the
# pull indefinitely (GHCR and LAN DHCP can be flaky for the first couple of
# minutes after boot, and slow connections may need many minutes per attempt).
# The sentinel is only written on success, so a reboot picks up where this
# left off.
if ! docker compose up -d; then
    echo "[$(date -Is)] up needs images not present locally — pulling from GHCR"
    attempt=0
    while true; do
        attempt=$((attempt + 1))
        if docker compose pull; then
            break
        fi
        echo "[$(date -Is)] pull attempt ${attempt} failed, retrying in 60 s"
        sleep 60
    done
    docker compose up -d
fi

touch "${SENTINEL}"
echo "[$(date -Is)] ftw-firstboot done"
