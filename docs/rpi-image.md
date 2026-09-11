# Raspberry Pi image

The recommended Raspberry Pi 4/5 installation uses Raspberry Pi Imager and the
FTW image repository. Imager downloads the image and lets you set hostname, SSH
credentials and Wi-Fi before writing the card.

## Install

1. Install [Raspberry Pi Imager](https://www.raspberrypi.com/software/) 2.0 or
   newer.
2. Open **App Options → Content Repository → Edit → Use custom URL**.
3. Paste:

   ```text
   https://github.com/srcfl/ftw/releases/download/rpi-installer/os_list.json
   ```

4. Click **Apply & Restart**.
5. Choose **Raspberry Pi 5** or **Raspberry Pi 4** on the device step.
6. Choose **FTW**, choose the SD card, and set hostname, SSH user/password and
   Wi-Fi in OS customisation.
7. Write the card, insert it and power on the Pi.
8. After first-boot provisioning, open `http://ftw.local/` on the same network.

Pick **Use custom URL**, not **Use custom file**. They are separate options in
the same dialog: **Use custom file** opens a file picker for a manifest already
on your computer and offers no field to paste a web address into. The FTW
manifest is hosted, so it belongs in **Use custom URL**.

`rpi-installer` is the permanent repository URL and is the recommended value
to save in Imager. Stable application releases also mirror this small file, so
links such as
`https://github.com/srcfl/ftw/releases/download/v1.3.0/os_list.json` remain
valid when copied from a version release. Both forms point at the same current
installer image; the image pulls the current stable containers on first boot.

Use a unique SSH password or key. FTW blocks unauthenticated mutations
through public hostnames. Config, logs, dumps, device identity, integration
status and local paths need the same local-or-token check. Live dashboard
reads (status, energy, prices, plan) stay unauthenticated. Keep the box on a
trusted LAN or behind an operator-managed, authenticated private/HTTPS proxy.
See [operations.md](operations.md#lan-and-api-access).

## Direct image fallback

The permanent installer release is:

```text
https://github.com/srcfl/ftw/releases/tag/rpi-installer
```

Download the newest `ftw-rpi4-arm64-*.img.xz` and flash it with Raspberry Pi
Imager (**Use custom**) or balenaEtcher. Both accept `.img.xz` directly.
Direct images do not get Imager's FTW customisation metadata, so Wi-Fi may need
the first-boot captive portal.

Do not flash the outer `.zip` downloaded from a GitHub Actions artifact;
extract it once and flash the enclosed `.img.xz`.

## First boot and network

First boot expands the filesystem, applies Imager/cloud-init settings, pulls
the stable FTW containers and starts the stack. Duration depends on the network.

With Ethernet, DHCP and mDNS should make `ftw.local` available. Without
preconfigured Wi-Fi or Ethernet, connect a phone/laptop to the open
`ftw-setup` access point and visit `http://192.168.42.1/`. Select the home
network and submit its password.

The setup wizard is available at:

```text
http://ftw.local/setup
http://<pi-ip>:8080/setup
```

If mDNS is blocked by the router, use the Pi's address from the router client
list.

## Diagnostics

```bash
ssh <user>@ftw.local
systemctl status ftw-firstboot
journalctl -u ftw-firstboot -b
docker compose -f /opt/ftw/docker-compose.yml ps
docker compose -f /opt/ftw/docker-compose.yml logs --tail=200 ftw
```

The first-boot service is idempotent. After correcting network or registry
access, retry with:

```bash
sudo systemctl restart ftw-firstboot
```

To recreate Wi-Fi onboarding:

```bash
sudo rm -f /var/lib/ftw/wifi-configured
sudo nmcli connection delete "<old SSID>"
sudo reboot
```

## Update

Use the update control in **Settings → System**, or:

```bash
cd /opt/ftw
sudo docker compose pull
sudo docker compose up -d
```

The installer image is independent of application releases. New images pull
the current stable containers on first boot; installed systems use the normal
beta/stable updater.

### Host OS security updates

The in-app updater covers FTW's own components — Core, Optimizer, drivers —
never the host underneath them. The image keeps the host patched with
`unattended-upgrades`: Debian security updates and the Raspberry Pi archive
(kernel, firmware, bootloader) apply automatically once a day. The Pi never
reboots on its own — a reboot stops dispatch — so an installed kernel takes
effect at the next reboot you choose.

The Docker engine comes from Docker's own apt repository, restored on first
boot, and updates only with a manual `sudo apt update && sudo apt upgrade`,
because an engine upgrade restarts the whole container stack.

Devices flashed from an image built before this policy can adopt it:

```bash
sudo apt-get update && sudo apt-get install -y unattended-upgrades
base=https://raw.githubusercontent.com/srcfl/ftw/master/deploy/pi-gen/stage-ftw/01-ftw-setup/files
sudo curl -fsSL "${base}/20auto-upgrades" -o /etc/apt/apt.conf.d/20auto-upgrades
sudo curl -fsSL "${base}/52ftw-unattended-upgrades" -o /etc/apt/apt.conf.d/52ftw-unattended-upgrades
echo "deb [arch=arm64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | sudo tee /etc/apt/sources.list.d/docker.list
```

## Build the image

Image provisioning lives under [`deploy/pi-gen`](../deploy/pi-gen):

```bash
deploy/pi-gen/build.sh
```

The output path and exact base image are defined by those scripts and the image
workflow. CI runs structural checks on changes; full images are built on the
installer schedule or explicit dispatch.

A custom repository replaces Imager's stock manifest rather than extending it,
so [`deploy/imager/os_list.json`](../deploy/imager/os_list.json) has to carry
both halves of the hardware contract itself:

- `imager.devices` supplies the device chooser. Imager 2.x builds the first
  wizard step from this list alone, and leaves **Next** disabled until a device
  is selected. A manifest without it dead-ends the wizard on an empty list.
- `os_list[].devices` marks FTW as compatible with those devices. Imager drops
  an entry with no `devices` tags whenever the selected device filters
  exclusively, and the field is required by
  [Imager's schema](https://github.com/raspberrypi/rpi-imager/blob/main/doc/json-schema/os-list-schema.json).

The tags are Imager's own (`pi5-64bit`, `pi4-64bit`); FTW ships arm64 only, so
it claims the 64-bit tags for Pi 4 and Pi 5. Keep the two lists consistent when
either changes — an entry tagged for a device the chooser never offers is
unreachable.
