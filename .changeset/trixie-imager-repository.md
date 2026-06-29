---
"forty-two-watts": minor
---

Migrate the Raspberry Pi SD-card image to Raspberry Pi OS Trixie with cloud-init, and publish a Raspberry Pi Imager repository JSON so Imager 2.0's customisation panel (hostname, SSH user/password, WiFi) works again.

Point Raspberry Pi Imager at `https://github.com/frahlg/forty-two-watts/releases/latest/download/os_list.json` (App Options → Content Repository → Use custom file) to flash "Forty-Two Watts" with full per-flash customisation. The repository JSON is rebuilt and uploaded on every release with the new image's URL, sizes, and verified checksum.

The on-image deploy directory moved from `/home/ftw/forty-two-watts` to `/opt/forty-two-watts` so a user-chosen account name can't orphan it. The `curl | install.sh` and manual docker-compose install paths are unaffected.
