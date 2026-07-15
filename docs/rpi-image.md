# Raspberry Pi SD-card image

The recommended way to install on a Raspberry Pi 4/5 is to point
**Raspberry Pi Imager** at the FTW image repository (a small
`os_list.json` file), pick **FTW**, and set your
**hostname, SSH user/password, and WiFi** right in Imager's
customisation panel before you write the card. Imager downloads the
image for you — you never fetch the `.img.xz` by hand. Drop the card
into a Pi, plug in power, and the dashboard is at `http://ftw.local/`
within ~90 s of first boot. No terminal, no manual install.

You *can* also download the `.img.xz` and flash it directly, but that
skips the customisation panel — the Pi then boots with default
credentials and WiFi has to be set up through the captive portal. It's
the fallback, not the recommended path.

---

## TL;DR (recommended)

1. Install [Raspberry Pi Imager](https://www.raspberrypi.com/software/) 2.0 or newer.
2. **App Options → Content Repository → EDIT → Use custom file** → paste the FTW repository URL, then **APPLY & RESTART**:
   ```
   https://github.com/srcfl/ftw/releases/latest/download/os_list.json
   ```
3. **CHOOSE OS → FTW**, **CHOOSE STORAGE → your SD card**, then set **hostname / SSH user+password / WiFi** in the customisation panel and **WRITE**.
4. Insert SD card → power on the Pi → wait ~90 s.
5. Open `http://ftw.local/` in any browser on the same network. (`:8080` also works — the image runs an nftables redirect from 80 to 8080 so the bare hostname is enough.)

That's it — Imager pulls the image itself, so you never download a file
by hand, and because WiFi is configured up front you need neither
Ethernet nor the captive portal. Prefer to flash a raw image instead?
See [the direct-download fallback](#download-the-image-directly-fallback)
(no customisation panel).

---

## What's on the image

| Component | Version |
|---|---|
| Base OS | **Raspberry Pi OS Lite** (64-bit, Debian Trixie) |
| Kernel | Stock Pi kernel (whatever the matching Pi OS Lite ships) |
| Init | systemd |
| Container engine | Docker CE + compose plugin (from Docker's official apt repo) |
| Network | NetworkManager + Avahi (mDNS) |
| WiFi onboarding | [`wifi-connect`](https://github.com/balena-os/wifi-connect) captive portal |
| Port redirect | nftables maps 80 -> 8080 so `http://ftw.local/` works without `:8080` |
| Stack | `FTW`, `mosquitto`, `ftw-updater` (pulled from GHCR on first boot) |

Image size: ~630 MB compressed, ~3.1 GB written to SD card (it then
grows to fill the card on first boot). Any 8 GB or larger card works;
16 GB+ recommended for headroom.

The base is **stock Raspberry Pi OS Lite** (Debian Trixie) with a
single custom stage layered on top. First-boot customisation
(hostname, SSH user, WiFi) is handled by **cloud-init** — the same
mechanism stock Raspberry Pi OS Trixie uses — when you set it in
Raspberry Pi Imager. SSH is enabled by default so you have a recovery
path if the dashboard ever gets stuck.

---

## Download the image directly (fallback)

> **Not the recommended path.** Downloading and flashing the raw
> `.img.xz` skips Imager's customisation panel, so the Pi boots with
> the default SSH credentials and you configure WiFi through the
> captive portal. To set hostname / SSH / WiFi before first boot, use
> the [repository flow](#tldr-recommended) instead — it downloads the
> image for you.

### Stable releases

Each tagged release publishes the image as a release asset:

```
https://github.com/srcfl/ftw/releases/latest
```

Look for `ftw-rpi4-arm64-vX.Y.Z.img.xz` under "Assets". This is a
**direct file download** — your browser saves the `.img.xz` and you
flash it as-is.

### PR previews (for testers)

Open pull requests that touch the image scaffolding auto-publish a
draft pre-release tagged `pr-<N>-image-preview`. Repo collaborators
can find these under [Releases → Drafts](https://github.com/srcfl/ftw/releases) — same direct
`.img.xz` download, no zip wrapper.

> **Don't use the GitHub Actions "Artifacts" download for flashing.**
> GitHub auto-wraps every workflow artifact in a `.zip`, and neither
> Imager nor Etcher knows how to look inside that wrapper. The
> Releases page (above) gives you the raw `.img.xz` directly.

---

## Flash

Both supported flashers handle `.img.xz` natively — they decompress
on the fly while writing to the card. **You do not need to extract
the file before flashing.**

### Path A — Raspberry Pi Imager with the FTW repository (recommended)

Raspberry Pi Imager 2.0 only shows the customisation panel (hostname,
SSH user/password, WiFi) for images it has metadata for. Point it at
the FTW repository so it does:

1. Install [Raspberry Pi Imager](https://www.raspberrypi.com/software/) (2.0 or newer).
2. **App Options → Content Repository → EDIT → Use custom file** → paste:
   ```
   https://github.com/srcfl/ftw/releases/latest/download/os_list.json
   ```
   → **APPLY & RESTART**. (CLI equivalent: `rpi-imager --repo <url>`.)
3. **CHOOSE OS** → **FTW** now appears in the list.
4. **CHOOSE STORAGE** → pick the SD card.
5. **NEXT** → when asked to apply OS customisation, set your **hostname**
   (keep `ftw` so the dashboard stays at `http://ftw.local/`), **SSH
   user/password**, and **WiFi** (SSID + password — the Pi joins your
   network at first boot, no captive portal needed). Then **WRITE**.

When it finishes, eject the card cleanly.

> **No repository?** You can still **CHOOSE OS → Use custom** and pick a
> downloaded `ftw-rpi4-arm64-*.img.xz` directly — but Imager then offers
> **no** customisation panel, so the Pi boots with default credentials
> and WiFi is configured via the captive portal (below).

### Path B — balenaEtcher

1. Install [balenaEtcher](https://etcher.balena.io/) (1.18 or newer
   handles `.img.xz` directly).
2. **Flash from file** → select the `.img.xz`.
3. **Select target** → SD card.
4. **Flash!**


WiFi is configured at first boot via the **captive portal** flow
(see below).

> **Common pitfall — wrong file.** If Etcher complains about a
> missing partition table or "not a valid disk image", you probably
> selected the GitHub Actions `.zip` wrapper instead of the `.img.xz`
> inside it. Download from the [Releases](https://github.com/srcfl/ftw/releases) page (not the Actions
> tab) — that gives you the raw `.img.xz`.

---

## First boot

Insert the SD card, plug in Ethernet (or rely on the captive
portal), connect power. Three things happen automatically:

1. **Partition resize.** The image is ~3.1 GB; Trixie grows the rootfs
   to fill your SD card on first boot (initramfs + `rpi-resize.service`).
   Adds ~30 s.
2. **cloud-init customisation.** If you set a hostname / SSH user / WiFi
   in Raspberry Pi Imager, cloud-init applies them now (from the
   `user-data` / `network-config` it wrote to the boot partition).
3. **First-boot service** (`ftw-firstboot.service`). Runs after
   cloud-init finishes, pulls the container images from GHCR
   (`FTW`, `mosquitto`, `ftw-updater`) and brings up the
   stack with `docker compose up -d` from `/opt/ftw`. Takes
   ~60–90 s on a decent connection.

After that, the dashboard is up and stays up across reboots.

---

## Connect to your network

### Ethernet — just plug it in

If the Pi is wired, nothing else to do. DHCP runs at boot, mDNS
publishes `ftw.local`, the dashboard is reachable.

### WiFi — captive portal (no Ethernet)

If WiFi wasn't pre-configured, ~30 s after boot the Pi exposes its
own access point named **`ftw-setup`** (no password).

From a phone or laptop:

1. Connect to `ftw-setup`.
2. A captive portal opens automatically. If it doesn't, visit
   `http://192.168.42.1/` in a browser.
3. Pick your home network → enter the password → submit.
4. The Pi joins the network, the AP disappears, the dashboard comes
   up at `http://ftw.local/` within 30–60 s.

iOS 17 and later occasionally suppress captive-portal popups —
manually open Safari to any `http://` (not `https://`) URL like
`http://example.com` and the portal will intercept it.

---

## Open the dashboard

```
http://ftw.local/
```

(`http://ftw.local:8080/` also works — the image runs an nftables
redirect at boot so port 80 lands on the same dashboard.)

First time you visit, you land in the setup wizard at `/setup`. Walk
through: location (for solar forecast), price zone, drivers (your
inverter / battery / EV charger), fuse capacity. The dashboard takes
over once you click "Finish".

If `ftw.local` doesn't resolve (some routers block mDNS, especially
mesh systems): find the Pi in your router's client list and use the
IP directly — `http://192.168.x.y/` (or `:8080`).

---

## Default credentials

| What | Default | How to change |
|---|---|---|
| SSH user | `ftw` | Imager customisation (via the FTW repository, Path A) |
| SSH password | `ftw` | Imager customisation, or `passwd` after first SSH |
| Hostname | `ftw` (mDNS: `ftw.local`) | Imager customisation |
| Dashboard | no auth on the LAN | (planned for a future release) |

The defaults exist so you have a recovery path if something goes
wrong — they're not meant for production. Override them in Imager's
customisation panel (which appears when you load the image via the FTW
repository — see Path A) when you flash for real use.

---

## Troubleshooting

### Dashboard doesn't load at `ftw.local`

SSH in:

```bash
ssh ftw@ftw.local                        # password: ftw
```

Diagnose:

```bash
systemctl status ftw-firstboot           # first-boot provisioner
journalctl -u ftw-firstboot -b           # its log (this boot)
tail -f /var/log/ftw-firstboot.log       # durable log
docker compose -f /opt/ftw/docker-compose.yml ps
```

If `ftw-firstboot` failed (bad network, GHCR outage), it's
idempotent — `systemctl restart ftw-firstboot` re-runs it.

### `ftw.local` doesn't resolve from my Mac/phone

Some routers (Eero, ASUS mesh, corporate Wi-Fi) block mDNS
broadcasts. Workarounds:

- Find the Pi's IP in the router's client list, use that instead.
- On macOS: `dns-sd -B _http._tcp` will list mDNS services if any
  are visible at all. If empty, mDNS is blocked at the router level.
- On Windows: install [Bonjour Print Services](https://support.apple.com/kb/DL999)
  to get mDNS resolution.

### Re-onboard WiFi from scratch

```bash
ssh ftw@ftw.local
sudo rm /var/lib/ftw/wifi-configured
sudo nmcli connection delete "<your old SSID>"
sudo reboot
```

Next boot, the captive portal comes up again.

### Reinstall from zero (drop all state)

```bash
ssh ftw@ftw.local
cd /opt/ftw
sudo docker compose down -v       # drops PV model, battery model,
                                  # price + load history, EV state
sudo rm -rf data mosquitto/data
sudo rm /var/lib/ftw/firstboot.done
sudo reboot
```

The Pi re-pulls images and runs the setup wizard from scratch.

### Dashboard works but I want to upgrade

Two ways:

- Click **Update** in the dashboard's Settings → System tab. The
  in-app sidecar (`ftw-updater`) handles the pull + restart.
- Or manually:

  ```bash
  ssh ftw@ftw.local
  cd /opt/ftw
  sudo docker compose pull && sudo docker compose up -d
  ```

---

## Building the image yourself

All image provisioning lives in `deploy/pi-gen/`. Any Linux host
with Docker (or macOS with Docker Desktop) can build:

```bash
deploy/pi-gen/build.sh
```

Output lands at `deploy/pi-gen/pi-gen/deploy/ftw-rpi4-arm64-*.img.xz`.
Build takes ~25–30 minutes on a decent laptop and uses ~15 GB of
working disk.

CI runs the same script on every PR that touches `deploy/pi-gen/**`
or the workflow itself (`.github/workflows/rpi-image-build.yml`),
plus on every tagged release (`.github/workflows/release.yml`).
