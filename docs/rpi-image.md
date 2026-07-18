# Raspberry Pi image

The recommended Raspberry Pi 4/5 installation uses Raspberry Pi Imager and the
FTW image repository. Imager downloads the image and lets you set hostname, SSH
credentials and Wi-Fi before writing the card.

## Install

1. Install [Raspberry Pi Imager](https://www.raspberrypi.com/software/) 2.0 or
   newer.
2. Open **App Options → Content Repository → Edit → Use custom file**.
3. Enter:

   ```text
   https://github.com/srcfl/ftw/releases/download/rpi-installer/os_list.json
   ```

4. Apply and restart Imager.
5. Choose **FTW**, choose the SD card, and set hostname, SSH user/password and
   Wi-Fi in OS customisation.
6. Write the card, insert it and power on the Pi.
7. After first-boot provisioning, open `http://ftw.local/` on the same network.

`rpi-installer` is the permanent repository URL and is the recommended value
to save in Imager. Stable application releases also mirror this small file, so
links such as
`https://github.com/srcfl/ftw/releases/download/v1.3.0/os_list.json` remain
valid when copied from a version release. Both forms point at the same current
installer image; the image pulls the current stable containers on first boot.

Use a unique SSH password or key. FTW blocks unauthenticated mutations through
public hostnames, but the dashboard still has no public read-authentication
boundary and should remain on a trusted LAN or behind an operator-managed,
authenticated private/HTTPS proxy. See [operations.md](operations.md#lan-and-api-access).

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

## Build the image

Image provisioning lives under `deploy/pi-gen`:

```bash
deploy/pi-gen/build.sh
```

The output path and exact base image are defined by those scripts and the image
workflow. CI runs structural checks on changes; full images are built on the
installer schedule or explicit dispatch.

The Imager repository entry intentionally has no `devices` filter. Custom
repositories do not receive Raspberry Pi Imager's stock hardware list, so a
filter would hide FTW from the chooser even though the image supports Pi 4/5.
