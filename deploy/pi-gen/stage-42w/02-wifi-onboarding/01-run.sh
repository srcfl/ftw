#!/bin/bash -e
# Runs OUTSIDE the chroot. wifi-connect's binary + UI tarballs are
# downloaded on the host and installed into the chroot via
# ${ROOTFS_DIR}/...; service enable + NetworkManager handoff happens
# inside on_chroot blocks.

WIFI_CONNECT_VERSION="${WIFI_CONNECT_VERSION:-v4.11.84}"
WIFI_CONNECT_BIN_URL="https://github.com/balena-os/wifi-connect/releases/download/${WIFI_CONNECT_VERSION}/wifi-connect-aarch64-unknown-linux-gnu.tar.gz"
WIFI_CONNECT_UI_URL="https://github.com/balena-os/wifi-connect/releases/download/${WIFI_CONNECT_VERSION}/wifi-connect-ui.tar.gz"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

curl -fsSL -o "${TMPDIR}/wc.tar.gz"    "${WIFI_CONNECT_BIN_URL}"
curl -fsSL -o "${TMPDIR}/wc-ui.tar.gz" "${WIFI_CONNECT_UI_URL}"

tar -xzf "${TMPDIR}/wc.tar.gz" -C "${TMPDIR}"
install -m 0755 "${TMPDIR}/wifi-connect" "${ROOTFS_DIR}/usr/local/sbin/wifi-connect"

install -d -m 0755                        "${ROOTFS_DIR}/usr/share/wifi-connect/ui"
tar -xzf "${TMPDIR}/wc-ui.tar.gz" -C "${ROOTFS_DIR}/usr/share/wifi-connect/ui"

install -m 0755 files/42w-wifi-onboarding            "${ROOTFS_DIR}/usr/local/sbin/42w-wifi-onboarding"
install -m 0644 files/42w-wifi-onboarding.service    "${ROOTFS_DIR}/etc/systemd/system/42w-wifi-onboarding.service"

on_chroot << 'EOF'
systemctl enable 42w-wifi-onboarding.service

# Make sure NetworkManager owns wlan0 uncontested so wifi-connect's
# AP-mode toggling doesn't race another supplicant. On trixie, netplan
# (NetworkManager renderer) is the source of truth and dhcpcd is
# normally absent — these commands are then a guarded no-op (|| true).
# We keep them as belt-and-suspenders in case a base bump reintroduces
# a stray dhcpcd on wlan0. cloud-init-applied WiFi (from the Imager
# network-config) lands as an NM connection via netplan, which the
# onboarding script already detects so the captive portal is skipped.
systemctl disable dhcpcd.service 2>/dev/null || true
systemctl mask    dhcpcd.service 2>/dev/null || true
EOF
