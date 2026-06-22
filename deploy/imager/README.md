# Raspberry Pi Imager — custom OS repository

`os_list.json` is a **Raspberry Pi Imager repository JSON** (V4 schema). It
describes the forty-two-watts SD-card image so that **Raspberry Pi Imager 2.0+**
shows the OS-customisation panel (hostname, SSH user/password, WiFi) for our
image — Imager only offers that panel for images it has metadata for, which a
bare `.img.xz` loaded via "Use custom" does not have.

## How users consume it

In Raspberry Pi Imager: **App Options → Content Repository → EDIT → Use custom
file**, paste the URL below, then **APPLY & RESTART** (or `rpi-imager --repo <url>`):

```
https://github.com/frahlg/forty-two-watts/releases/latest/download/os_list.json
```

`releases/latest/download/...` always redirects to the newest release's asset,
so the URL never changes between versions.

## This file is a template

The committed copy is a **template**: `url`, `extract_size`, `extract_sha256`,
`image_download_size`, and `release_date` carry placeholder/zero values. The
`rpi-image` job in `.github/workflows/release-assets.yml` renders these with the
real values for each release (the `.img.xz` URL, its compressed + decompressed
sizes, and the SHA-256 of the decompressed image — which Imager verifies after
writing) and uploads the rendered `os_list.json` as a release asset.

`init_format` is `cloudinit-rpi` because the image is Raspberry Pi OS Trixie with
cloud-init (Imager writes `user-data` / `network-config` / `meta-data` to the boot
partition). If the image base ever changes, update `init_format` accordingly
(`systemd` for the legacy `firstrun.sh` path on bookworm).

## Why there is no `devices` filter

The OS entry deliberately omits the `devices` array (e.g. `["pi4-64bit",
"pi5-64bit"]`). When Imager loads a **custom** repository via `--repo`, it uses
that repository for the OS list but does **not** get the hardware/device list
that the stock repository ships — so the "CHOOSE DEVICE" picker is empty. A
device-filtered entry then never appears, because no matching device can be
selected. Omitting `devices` makes "Forty-Two Watts" show regardless of the
selected device. The image only supports Pi 4/5 (64-bit), which the `name` and
`description` already state. **Do not re-add `devices`** — it silently hides the
entry in the custom-repo flow.
