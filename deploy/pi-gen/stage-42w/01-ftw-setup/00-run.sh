#!/bin/bash -e
# Runs OUTSIDE the chroot. File manipulation uses ${ROOTFS_DIR}/...
# paths; chroot operations go through on_chroot. pi-gen's own
# stages follow this same pattern (see stage2/01-sys-tweaks) —
# *-run-chroot.sh scripts are piped to on_chroot as stdin and
# therefore can't read the files/ directory, hence the split.

# Docker — explicit apt repo + minimal package set. Replaces
# `curl get.docker.com | sh` which pulls docker-ce-rootless-extras +
# docker-buildx-plugin + docker-model-plugin (~300 MB combined). We
# don't run rootless, don't build images on the Pi, and have no AI
# workloads — so those stay out. The bare four packages below are
# what `docker compose up -d` against a pulled image actually needs.
# Same Docker apt repo URL and pinning approach the convenience
# script uses, so we stay on the same engine version stream.
on_chroot << 'EOF'
set -e
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=arm64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable" \
    > /etc/apt/sources.list.d/docker.list
apt-get -qq update
DEBIAN_FRONTEND=noninteractive apt-get -y -qq install --no-install-recommends \
    docker-ce docker-ce-cli containerd.io docker-compose-plugin
systemctl enable docker.service
systemctl enable avahi-daemon.service
systemctl enable NetworkManager.service
# pi-gen's export-image stage runs another apt update under qemu. Leaving
# Docker's third-party apt source enabled has repeatedly OOMed that step on
# GitHub hosted runners after Docker is already installed. App updates pull
# containers from GHCR, so the image build does not need this repo afterward.
rm -f /etc/apt/sources.list.d/docker.list
# /etc/hosts entry prevents sudo's "unable to resolve host 42w"
# warning on first boot. pi-gen writes /etc/hostname from
# TARGET_HOSTNAME but leaves /etc/hosts at the stock Raspberry Pi
# OS template (which hard-codes `raspberrypi`).
sed -i 's/^127\.0\.1\.1.*/127.0.1.1\t42w/' /etc/hosts
EOF

# init=firstboot on first boot — without stage2, nobody else injects
# this into cmdline.txt. The firstboot script ships with
# raspberrypi-sys-mods and handles partition resize, ssh enable, and
# userconf-pi processing on the very first boot, then removes itself
# from cmdline.txt and reboots. Mirrors what pi-gen's
# stage2/01-sys-tweaks/01-run.sh does on a stock Lite build.
# Idempotent — re-running the build won't double-inject.
CMDLINE="${ROOTFS_DIR}/boot/firmware/cmdline.txt"
if [ -f "${CMDLINE}" ] && ! grep -q "raspberrypi-sys-mods/firstboot" "${CMDLINE}"; then
    sed -i 's| rootwait| init=/usr/lib/raspberrypi-sys-mods/firstboot rootwait|' "${CMDLINE}"
fi

# Deploy directory: docker-compose.yml lives here and
# `docker compose up -d` runs from it on first boot.
#
# Path: /home/ftw/ — the user `ftw` is created at BUILD TIME by
# stage1 (FIRST_USER_NAME=ftw in deploy/pi-gen/config), so its
# home directory exists and is owned by ftw:ftw before stage-42w
# runs. There's no `pi → ftw` rename in our flow — the rename
# wizard pi-gen sets up is for END USERS who customize the
# username via Imager's userconf.txt (handled by userconf-pi at
# first boot, see below).
#
# Earlier builds (commit 6aa5a9d, "trim image") wrote to
# /home/pi/ on a now-disproved theory that a rename would move
# files. On real hardware, /home/pi/ ended up as a stale
# root-owned directory and ftw-firstboot died with
# "cd: /home/ftw/forty-two-watts: No such file or directory".
#
# data/ is chowned 100:101 because the in-container ftw user
# (alpine `adduser -S`) needs to own it before SQLite can create
# state.db. Same UID/GID mapping as scripts/install.sh.
install -d -m 0755                    "${ROOTFS_DIR}/home/ftw/forty-two-watts"
install -d -m 0755 -o 100 -g 101      "${ROOTFS_DIR}/home/ftw/forty-two-watts/data"
install -d -m 0755                    "${ROOTFS_DIR}/home/ftw/forty-two-watts/mosquitto"
install -d -m 0755                    "${ROOTFS_DIR}/home/ftw/forty-two-watts/mosquitto/config"

install -m 0644 files/docker-compose.yml    "${ROOTFS_DIR}/home/ftw/forty-two-watts/docker-compose.yml"
install -m 0644 files/mosquitto.conf        "${ROOTFS_DIR}/home/ftw/forty-two-watts/mosquitto/config/mosquitto.conf"

install -m 0755 files/firstboot.sh          "${ROOTFS_DIR}/usr/local/sbin/ftw-firstboot"
install -m 0644 files/firstboot.service     "${ROOTFS_DIR}/etc/systemd/system/ftw-firstboot.service"

# Sudoers fragment for ftw — without stage2, no `010_pi-nopasswd`
# file gets installed, so the ftw user has no admin rights at all.
# Mirror the pi-OS-Lite default of passwordless sudo for the
# admin user; operators expect to be able to SSH in and diagnose
# without remembering a password they probably overrode in Imager.
install -m 0440 files/010_ftw-nopasswd      "${ROOTFS_DIR}/etc/sudoers.d/010_ftw-nopasswd"

# Chown everything to ftw so `ls -la` and edit-without-sudo work
# when an operator SSHes in. data/ keeps its 100:101 ownership for
# the in-container user. Run inside chroot so /etc/passwd resolves
# `ftw` correctly.
on_chroot << 'EOF'
chown -R ftw:ftw /home/ftw/forty-two-watts
chown 100:101 /home/ftw/forty-two-watts/data
systemctl enable ftw-firstboot.service
EOF
