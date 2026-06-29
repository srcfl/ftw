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

cd /opt/forty-two-watts

# Retry loop: GHCR and general LAN DHCP can be flaky for the first
# couple of minutes after boot. Back off linearly rather than
# exponentially — the failure mode we're covering (slow DHCP lease,
# pending captive-portal login) resolves in single-digit minutes.
for attempt in 1 2 3 4 5 6; do
    if docker compose pull; then
        break
    fi
    echo "[$(date -Is)] pull attempt ${attempt}/6 failed, sleeping 15 s"
    sleep 15
    if [ "${attempt}" = "6" ]; then
        echo "[$(date -Is)] pull gave up — service will retry on next boot"
        exit 1
    fi
done

docker compose up -d

touch "${SENTINEL}"
echo "[$(date -Is)] ftw-firstboot done"
