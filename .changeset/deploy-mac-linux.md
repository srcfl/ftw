---
"forty-two-watts": minor
---

**Document and support running off a Mac mini or a generic Linux server.**
The Docker stack already ran on any Linux box via `docker-compose.yml`,
but that file uses `network_mode: host` — a Linux-kernel feature that, on
macOS, binds to the Docker Desktop VM rather than the Mac, leaving the
dashboard unreachable and silently breaking device discovery.

- **`docker-compose.macos.yml`** — a self-contained macOS compose file
  that swaps host networking for bridge networking with published ports
  (`8080`, `1883`). The app reaches the embedded broker by service name
  (`mosquitto:1883`), and the `ftw-updater` sidecar is wired to the
  macOS file so the in-app Update/Restart buttons recreate the right
  containers.
- **`scripts/install-macos.sh`** — one-shot macOS installer: verifies
  Docker Desktop is up, lays out `~/forty-two-watts`, fetches the macOS
  compose file + broker config, and brings the stack up. The Linux
  installer now bails early with a pointer when run on macOS.
- **`docs/deploy-platforms.md`** — new guide covering both the generic
  Linux server path (Ubuntu/NUC/VM: install, `ufw`, device-identity
  caveats) and the Mac mini path (Docker Desktop networking caveats:
  point MQTT at `mosquitto`, use explicit driver IPs since mDNS/broadcast
  don't cross the VM boundary, keep-it-running tips). `docker-compose.yml`
  and `operations.md` now cross-reference it.
