#!/bin/bash
# First-boot provisioner for forty-two-watts.
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

cd /home/ftw/forty-two-watts

# Retry loop: GHCR and general LAN DHCP can be flaky for the first
# couple of minutes after boot, and slow connections may need many
# minutes per attempt. Retry indefinitely — the sentinel is only
# written on success, so a reboot will pick up where this left off.
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

touch "${SENTINEL}"
echo "[$(date -Is)] ftw-firstboot done"
