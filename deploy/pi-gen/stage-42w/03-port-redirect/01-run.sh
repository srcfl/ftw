#!/bin/bash -e
# Install the nftables rules file + the systemd oneshot that loads
# it at boot. See files/42w-port-redirect.nft for why this exists
# (compose stack uses host networking, so docker port mapping isn't
# available; redirecting at the kernel keeps the app on 8080 and
# makes 80 reachable for free).

install -m 0644 files/42w-port-redirect.nft       "${ROOTFS_DIR}/etc/42w-port-redirect.nft"
install -m 0644 files/42w-port-redirect.service   "${ROOTFS_DIR}/etc/systemd/system/42w-port-redirect.service"

on_chroot << 'EOF'
systemctl enable 42w-port-redirect.service
EOF
