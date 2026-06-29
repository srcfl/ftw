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
# Derive the Debian codename from the target rootfs (trixie on the
# current image) rather than hardcoding it — keeps this correct across
# a pi-gen base bump. This runs INSIDE the chroot (the heredoc is
# single-quoted, so $(…) is evaluated here, not on the build host),
# so /etc/os-release is the image's.
echo "deb [arch=arm64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
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

# NO manual cmdline.txt first-boot injection on trixie.
#
# On bookworm we hand-injected `init=/usr/lib/raspberrypi-sys-mods/firstboot`
# (because we skipped stage2). Trixie REPLACED that mechanism: the
# `init=` first-boot path is gone, resize now runs via initramfs +
# rpi-resize.service, and the customisation hook is `systemd.run=`.
# Removing/forcing `init=` on trixie causes a boot loop. We now include
# stage2 (see STAGE_LIST in config), so pi-gen sets the correct trixie
# first-boot/resize cmdline + cloud-init datasource itself — we must not
# fight it.

# Deploy directory: docker-compose.yml lives here and
# `docker compose up -d` runs from it on first boot.
#
# Path: /opt/forty-two-watts — a FIXED system path, deliberately NOT
# under a user home. On trixie + cloud-init, Raspberry Pi Imager lets
# the user set their own account name; cloud-init may create/rename the
# uid-1000 user, which would orphan a deploy dir under /home/ftw/. A
# fixed /opt path is immune to whatever the user names their account.
#
# Self-update stays correct across this move: the ftw-updater sidecar
# derives its compose path from ${PWD} at `docker compose up -d` time
# (see docker-compose.yml) and firstboot.sh cd's here, so the updater
# records /opt/forty-two-watts automatically. COMPOSE_PROJECT_NAME
# stays the literal `forty-two-watts`.
#
# data/ is owned 100:101 because the in-container ftw user (alpine
# `adduser -S`) needs to own it before SQLite can create state.db.
# Same UID/GID mapping as scripts/install.sh.
install -d -m 0755                    "${ROOTFS_DIR}/opt/forty-two-watts"
install -d -m 0755 -o 100 -g 101      "${ROOTFS_DIR}/opt/forty-two-watts/data"
install -d -m 0755                    "${ROOTFS_DIR}/opt/forty-two-watts/mosquitto"
install -d -m 0755                    "${ROOTFS_DIR}/opt/forty-two-watts/mosquitto/config"

install -m 0644 files/docker-compose.yml    "${ROOTFS_DIR}/opt/forty-two-watts/docker-compose.yml"
install -m 0644 files/mosquitto.conf        "${ROOTFS_DIR}/opt/forty-two-watts/mosquitto/config/mosquitto.conf"

install -m 0755 files/firstboot.sh          "${ROOTFS_DIR}/usr/local/sbin/ftw-firstboot"
install -m 0644 files/firstboot.service     "${ROOTFS_DIR}/etc/systemd/system/ftw-firstboot.service"

# Sudoers fragment for ftw. Stage2 installs an `010_<user>-nopasswd` for
# the build-time FIRST_USER (ftw); we drop this belt-and-suspenders copy
# so the default ftw recovery account keeps passwordless sudo even if a
# pi-gen internal changes. A user who renames the account via Imager gets
# their NOPASSWD sudo from cloud-init's `users:` directive instead.
install -m 0440 files/010_ftw-nopasswd      "${ROOTFS_DIR}/etc/sudoers.d/010_ftw-nopasswd"

# /opt/forty-two-watts stays root-owned (operators edit via passwordless
# sudo); data/ keeps its 100:101 ownership for the in-container user,
# already set numerically by `install -d -o 100 -g 101` above.
# Mask the Raspberry Pi OS first-run user wizard. On a headless,
# cloud-init-provisioned image it is both redundant and fatal: the `ftw`
# account (or the Imager-customised account) already exists, yet
# userconfig.service still runs `userconf-service`, which blocks on a
# `whiptail "Please enter new username:"` dialog on tty8. It is
# Type=oneshot WantedBy=multi-user.target, so that never-completing dialog
# wedges multi-user.target — and with it ftw-firstboot.service (ordered
# After=cloud-final.service, itself queued behind the wizard) — so the
# stack is never pulled and the dashboard never comes up. Masking it lets
# a no-customisation flash boot through to multi-user.target.
on_chroot << 'EOF'
systemctl enable ftw-firstboot.service
systemctl mask userconfig.service
EOF
