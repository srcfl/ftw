# Raspberry Pi SD-card image

A pre-built `.img.xz` ships with every release. Flash it to an SD
card, drop the card into a Raspberry Pi 4, plug in power + Ethernet
(or follow the WiFi-onboarding flow below), and the dashboard is at
`http://42w.local/` within ~90 s of first boot. No terminal, no
manual install.

This is the recommended path for new users.

---

## TL;DR

1. Download `42w-rpi4-arm64-vX.Y.Z.img.xz` from [Releases](https://github.com/frahlg/forty-two-watts/releases/latest).
2. Flash to an SD card with [Raspberry Pi Imager](https://www.raspberrypi.com/software/) (recommended) or [balenaEtcher](https://etcher.balena.io/). Both handle `.img.xz` natively — no need to decompress first.
3. Insert SD card → power on the Pi → wait ~90 s.
4. Open `http://42w.local/` in any browser on the same network. (`:8080` also works — the image runs an nftables redirect from 80 to 8080 so the bare hostname is enough.)

If you don't have Ethernet, see [WiFi onboarding](#connect-to-your-network).

---

## What's on the image

| Component | Version |
|---|---|
| Base OS | **Raspberry Pi OS Lite** (64-bit, Debian Bookworm) |
| Kernel | Stock Pi kernel (whatever the matching Pi OS Lite ships) |
| Init | systemd |
| Container engine | Docker CE + compose plugin (from Docker's official apt repo) |
| Network | NetworkManager + Avahi (mDNS) |
| WiFi onboarding | [`wifi-connect`](https://github.com/balena-os/wifi-connect) captive portal |
| Port redirect | nftables maps 80 -> 8080 so `http://42w.local/` works without `:8080` |
| Stack | `forty-two-watts`, `mosquitto`, `ftw-updater` (pulled from GHCR on first boot) |

Image size: ~410 MB compressed, ~2.4 GB written to SD card. Any 8 GB
or larger card works; 16 GB+ recommended for headroom.

The base is **stock Raspberry Pi OS Lite** with stage2 trimmed off
and a single custom stage layered on top. SSH is enabled by default
so you have a recovery path if the dashboard ever gets stuck.

---

## Download

### Stable releases (recommended)

Each tagged release publishes the image as a release asset:

```
https://github.com/frahlg/forty-two-watts/releases/latest
```

Look for `42w-rpi4-arm64-vX.Y.Z.img.xz` under "Assets". This is a
**direct file download** — your browser saves the `.img.xz` and you
flash it as-is.

### PR previews (for testers)

Open pull requests that touch the image scaffolding auto-publish a
draft pre-release tagged `pr-<N>-image-preview`. Repo collaborators
can find these under [Releases → Drafts](https://github.com/frahlg/forty-two-watts/releases) — same direct
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

### Path A — Raspberry Pi Imager (recommended)

1. Install [Raspberry Pi Imager](https://www.raspberrypi.com/software/).
2. **CHOOSE OS** → scroll to bottom → **Use custom** → select your
   downloaded `42w-rpi4-arm64-*.img.xz`.
3. **CHOOSE STORAGE** → pick the SD card.
4. Click the **gear icon** (advanced options). Recommended settings:
   - **Hostname**: leave as `42w` (so the dashboard URL stays
     `http://42w.local:8080/`).
   - **Set username and password**: optional. The image ships with
     `ftw / fortytwowatts` for SSH recovery; override here if you
     want something else.
   - **Configure wireless LAN**: paste your WiFi SSID + password.
     The Pi connects directly at first boot — no captive portal
     needed. Skip this if you're using Ethernet.
   - **Set locale settings**: pick your timezone.
5. **WRITE**. Imager handles xz decompression + verification.

When it finishes, eject the card cleanly.

### Path B — balenaEtcher

1. Install [balenaEtcher](https://etcher.balena.io/) (1.18 or newer
   handles `.img.xz` directly).
2. **Flash from file** → select the `.img.xz`.
3. **Select target** → SD card.
4. **Flash!**

Etcher doesn't have Imager's pre-configuration panel, so:
- The Pi will boot with **default credentials** (`ftw` / `fortytwowatts`).
- WiFi is configured at first boot via the **captive portal** flow
  (see below). If you want to skip the portal, use Imager instead
  and pre-configure WiFi.

> **Common pitfall — wrong file.** If Etcher complains about a
> missing partition table or "not a valid disk image", you probably
> selected the GitHub Actions `.zip` wrapper instead of the `.img.xz`
> inside it. Download from the [Releases](https://github.com/frahlg/forty-two-watts/releases) page (not the Actions
> tab) — that gives you the raw `.img.xz`.

---

## First boot

Insert the SD card, plug in Ethernet (or rely on the captive
portal), connect power. Two things happen automatically:

1. **Partition resize.** The image is ~2.4 GB; the resize step grows
   the rootfs to fill your SD card on the very first boot. The Pi
   reboots once to pick up the resized partition. Adds ~30 s.
2. **First-boot service** (`ftw-firstboot.service`). Pulls the
   container images from GHCR (`forty-two-watts`, `mosquitto`,
   `ftw-updater`), brings up the stack with `docker compose up -d`.
   Takes ~60–90 s on a decent connection.

After that, the dashboard is up and stays up across reboots.

---

## Connect to your network

### Ethernet — just plug it in

If the Pi is wired, nothing else to do. DHCP runs at boot, mDNS
publishes `42w.local`, the dashboard is reachable.

### WiFi — pre-configured in Imager (best)

If you used Path A above with WiFi credentials filled in, the Pi
joins your network at first boot. Skip to [Open the dashboard](#open-the-dashboard).

### WiFi — captive portal (Etcher path or no Ethernet)

If WiFi wasn't pre-configured, ~30 s after boot the Pi exposes its
own access point named **`42w-setup`** (no password).

From a phone or laptop:

1. Connect to `42w-setup`.
2. A captive portal opens automatically. If it doesn't, visit
   `http://192.168.42.1/` in a browser.
3. Pick your home network → enter the password → submit.
4. The Pi joins the network, the AP disappears, the dashboard comes
   up at `http://42w.local/` within 30–60 s.

iOS 17 and later occasionally suppress captive-portal popups —
manually open Safari to any `http://` (not `https://`) URL like
`http://example.com` and the portal will intercept it.

---

## Open the dashboard

```
http://42w.local/
```

(`http://42w.local:8080/` also works — the image runs an nftables
redirect at boot so port 80 lands on the same dashboard.)

First time you visit, you land in the setup wizard at `/setup`. Walk
through: location (for solar forecast), price zone, drivers (your
inverter / battery / EV charger), fuse capacity. The dashboard takes
over once you click "Finish".

If `42w.local` doesn't resolve (some routers block mDNS, especially
mesh systems): find the Pi in your router's client list and use the
IP directly — `http://192.168.x.y/` (or `:8080`).

---

## Default credentials

| What | Default | How to change |
|---|---|---|
| SSH user | `ftw` | Imager advanced options |
| SSH password | `fortytwowatts` | Imager advanced options, or `passwd` after first SSH |
| Hostname | `42w` (mDNS: `42w.local`) | Imager advanced options |
| Dashboard | no auth on the LAN | (planned for a future release) |

The defaults exist so you have a recovery path if something goes
wrong — they're not meant for production. Override them in Imager
when you flash for real use.

---

## Troubleshooting

### Dashboard doesn't load at `42w.local`

SSH in:

```bash
ssh ftw@42w.local                        # password: fortytwowatts
```

Diagnose:

```bash
systemctl status ftw-firstboot           # first-boot provisioner
journalctl -u ftw-firstboot -b           # its log (this boot)
tail -f /var/log/ftw-firstboot.log       # durable log
docker compose -f /home/ftw/forty-two-watts/docker-compose.yml ps
```

If `ftw-firstboot` failed (bad network, GHCR outage), it's
idempotent — `systemctl restart ftw-firstboot` re-runs it.

### `42w.local` doesn't resolve from my Mac/phone

Some routers (Eero, ASUS mesh, corporate Wi-Fi) block mDNS
broadcasts. Workarounds:

- Find the Pi's IP in the router's client list, use that instead.
- On macOS: `dns-sd -B _http._tcp` will list mDNS services if any
  are visible at all. If empty, mDNS is blocked at the router level.
- On Windows: install [Bonjour Print Services](https://support.apple.com/kb/DL999)
  to get mDNS resolution.

### Re-onboard WiFi from scratch

```bash
ssh ftw@42w.local
sudo rm /var/lib/ftw/wifi-configured
sudo nmcli connection delete "<your old SSID>"
sudo reboot
```

Next boot, the captive portal comes up again.

### Reinstall from zero (drop all state)

```bash
ssh ftw@42w.local
cd /home/ftw/forty-two-watts
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
  ssh ftw@42w.local
  cd /home/ftw/forty-two-watts
  sudo docker compose pull && sudo docker compose up -d
  ```

---

## Building the image yourself

All image provisioning lives in `deploy/pi-gen/`. Any Linux host
with Docker (or macOS with Docker Desktop) can build:

```bash
deploy/pi-gen/build.sh
```

Output lands at `deploy/pi-gen/pi-gen/deploy/42w-rpi4-arm64-*.img.xz`.
Build takes ~25–30 minutes on a decent laptop and uses ~15 GB of
working disk.

CI runs the same script on every PR that touches `deploy/pi-gen/**`
or the workflow itself (`.github/workflows/rpi-image-build.yml`),
plus on every tagged release (`.github/workflows/release.yml`).
