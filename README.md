# FTW

<img src="web/logo.jpg" alt="FTW" width="120" align="right">

> Local-first home energy coordination.

FTW is an open-source local energy runtime for solar, batteries, grid and EV
charging. Its core runs as a single Go binary on a Raspberry Pi or Linux host,
coordinates devices through Lua drivers, and keeps the control loop local. The
optional full mathematical optimizer uses Python/CVXPY; official containers
include that runtime and native installs fall back safely to the Go planner when
it is unavailable.

FTW is maintained by Sourceful Labs AB and project contributors. It is a
self-hosted open-source project, not a hosted Sourceful service; the local
control path does not depend on Sourceful cloud services.

> **Upgrading an existing Forty Two Watts installation?** Follow the
> step-by-step [legacy upgrade guide in Swedish and English](docs/upgrade-from-legacy.md).
> Its migration command moves older Docker Compose and local-development
> layouts to the official Sourceful images while preserving the existing
> service, directory, configuration, history, and device data.

The project is active and runs on real hardware, but API and config fields
can still change before a stable 1.0 release. Version numbers come from
git tags and `package.json`; use the GitHub releases page for the latest
published build.

## What It Does

- **Self-consumption**: batteries discharge to cover household load, charge
  from PV surplus, and keep the site meter near the configured target.
- **MPC planning**: a 48-hour planner uses spot prices, weather, PV, load,
  and battery state to choose charge, discharge, hold, or export targets.
- **EV and V2X awareness**: EV charging is treated as load, and V2X chargers
  can emit bidirectional vehicle power without confusing stationary batteries.
- **Calendar planning (CalDAV)**: add events in your normal calendar app and
  FTW turns them into planner intents — *Away* / *Vacation* conserves battery,
  *Charge car 80%* sets an EV departure deadline — and publishes charging
  windows + EVSE usage history back to a calendar you can subscribe to. FTW
  hosts CalDAV itself, in-process, via [`emersion/go-webdav`](https://github.com/emersion/go-webdav)
  — no extra container, recurring events supported, and it works as a Home
  Assistant add-on. See [`docs/caldav-integration.md`](docs/caldav-integration.md).
- **Multi-device control**: multiple meters, inverters, batteries, PV-only
  devices, and chargers can run side by side.
- **Local operation**: the control loop does not depend on a cloud service.
  Prices, weather, notifications, and cloud drivers degrade independently.

## Supported Devices

Drivers are plain Lua files under [`drivers/`](drivers/). The in-app catalog
is generated from each driver's `DRIVER` metadata block, and
[`docs/driver-catalog.md`](docs/driver-catalog.md) mirrors that metadata for
humans. The driver list should not be maintained as a number in this README.

Current bundled driver families include:

| Category | Examples |
|---|---|
| Hybrid inverters | Sungrow, Ferroamp, Solis, Huawei, Deye, SMA, Fronius, GoodWe, Growatt, Sofar, Victron, Kostal |
| PV and meters | SolarEdge, SMA PV, Pixii PV, Eastron SDM630, Fronius Smart Meter, Tibber Pulse, Zuidwijk P1, Sourceful Zap |
| Batteries | Ferroamp, Pixii, sonnen, hybrid inverter batteries |
| EV and V2X | Easee, CTEK Chargestorm, Tesla Vehicle, Ferroamp DC2 V2X, Ambibox V2X |

Adding a new device starts with
[`docs/writing-a-driver.md`](docs/writing-a-driver.md).

## Quick Start

### Option A: Raspberry Pi SD-card image

Recommended: point **Raspberry Pi Imager** at the FTW image repository
(**App Options → Content Repository → Use custom file**):

```
https://github.com/srcfl/ftw/releases/latest/download/os_list.json
```

Then pick **FTW**, set your hostname / SSH user / Wi-Fi in the
customisation panel, and write — Imager downloads the image for you. Boot the
Pi and open `http://ftw.local/`.

You can instead download the `ftw-rpi4-arm64-vX.Y.Z.img.xz` release asset and
flash it directly, but that skips the customisation panel (default
credentials, Wi-Fi via the `ftw-setup` captive portal) — not recommended.

Full walkthrough: [`docs/rpi-image.md`](docs/rpi-image.md).

### Option B: Docker installer

On Raspberry Pi OS, Debian, or Ubuntu:

```bash
curl -fsSL https://raw.githubusercontent.com/srcfl/ftw/master/scripts/install.sh | bash
```

Then open `http://<your-pi>:8080/setup`.

### Option C: Home Assistant OS add-on

If you run Home Assistant OS or HA Supervised, install the add-on from
[`erikarenhill/ha-addon-forty-two-watts`](https://github.com/erikarenhill/ha-addon-forty-two-watts).

### Option D: Build from source

Prerequisites: Go 1.26+, a Linux/Raspberry Pi target, and at least one
supported device or simulator.

```bash
git clone https://github.com/srcfl/ftw
cd ftw

make dev          # simulators + app at http://localhost:8080
make test         # unit + integration tests
make build-arm64  # cross-compile for Raspberry Pi
```

The first `make dev` copies the tracked simulator template to the gitignored
`config.local.yaml`, starts both simulators, and opens the app on port 8080.
For real hardware, copy `config.example.yaml` to `config.yaml` and fill in your
device capabilities instead.

## How It Works

```
config.yaml
    |
    v
Lua drivers: Modbus / MQTT / HTTP / WebSocket / raw TCP
    |
    v
Telemetry store: latest readings, driver health, metric queue
    |
    v
Control loop: PI, dispatch splitting, slew, fuse guard, watchdog
    |
    v
MPC planner + PV/load/price twins + SQLite state
    |
    v
HTTP API, dashboard, Home Assistant bridge, notifications
```

All power values above the driver boundary use the same site convention:
positive W means energy flowing into the site across the grid-meter
boundary. Read [`docs/site-convention.md`](docs/site-convention.md) before
touching power math.

## Remote Access

> **Transition status:** local control is unaffected, but the new Sourceful
> relay/TURN endpoints are not yet declared operational. Treat remote access as
> unavailable unless the deployment runbook has been completed and verified;
> this infrastructure cutover is tracked separately from the repository
> migration.

Remote access is opt-in and still keeps the home site local-first. Enable it
from the local dashboard under **Settings -> Access**, save, and restart when
prompted. The Pi then registers an opaque, high-entropy `site_id` with the
public relay and publishes only the minimal information needed for a browser to
find that Pi.

Once that infrastructure has been provisioned and verified, the public
`home.fortytwowatts.com` route works in three layers:

1. The relay serves a small loader and owner-access pages from the
   `ftw-relay-web.tar.gz` release asset.
2. After a browser unlocks its encrypted local directory, static dashboard
   files are fetched from the selected Pi through the relay route.
3. Owner API calls, login, status, prices, history, plans, settings, and
   control commands go over the strict WebRTC DataChannel to the Pi.

The relay is therefore only a blind router and bootstrap host. It does not
store the dashboard app bundle, does not terminate owner sessions, does not
receive `ftw_owner` cookies, and does not inspect passkeys or owner data.
Passkeys, remembered browser keys, and active sessions are managed locally in
the Access tab, where they can also be revoked.

First setup is a one-time bootstrap: the local Access screen shows a QR/link and
PIN only before the first passkey exists, and only after the relay has accepted
the live setup invitation. After one passkey is enrolled, add or revoke access
from **Settings -> Access** while signed in.

Relay operators should install the relay bootstrap bundle from each release,
not copy the Pi dashboard `web/` directory to the relay. Deployment details:
[`docs/relay-deploy.md`](docs/relay-deploy.md).

## Documentation

**Get started**

- [`docs/rpi-image.md`](docs/rpi-image.md) - SD-card image and captive portal
- [`docs/setup-guide/`](docs/setup-guide/) - alternative generic Raspberry Pi OS + Docker setup
- [`docs/configuration.md`](docs/configuration.md) - YAML config reference
- [`docs/driver-catalog.md`](docs/driver-catalog.md) - bundled Lua drivers

**Run it**

- [`docs/operations.md`](docs/operations.md) - deploy, backup, logs, recovery
- [`docs/self-update.md`](docs/self-update.md) - Docker updater sidecar
- [`MIGRATION.md`](MIGRATION.md) - former-name compatibility and upgrade notes
- [`docs/ha-integration.md`](docs/ha-integration.md) - MQTT autodiscovery
- [`docs/caldav-integration.md`](docs/caldav-integration.md) - calendar planner constraints (CalDAV)
- [`docs/safety.md`](docs/safety.md) - watchdog, clamps, fuse guard

**Understand it**

- [`docs/architecture.md`](docs/architecture.md) - system map and data flow
- [`docs/site-convention.md`](docs/site-convention.md) - sign convention
- [`docs/ml-models.md`](docs/ml-models.md) - PV, load, and price twins
- [`docs/mpc-planner.md`](docs/mpc-planner.md) - planner strategy details
- [`docs/battery-models.md`](docs/battery-models.md) - ARX/RLS battery models
- [`docs/api.md`](docs/api.md) - HTTP API reference

**Build with it**

- [`docs/writing-a-driver.md`](docs/writing-a-driver.md) - Lua driver guide
- [`docs/host-api.md`](docs/host-api.md) - `host.*` Lua capability reference
- [`docs/testing-drivers-live.md`](docs/testing-drivers-live.md) - live driver testing
- [`docs/testing.md`](docs/testing.md) - repo test guide
- [`docs/development.md`](docs/development.md) - local development loop

Historical plans and early TODOs live under [`docs/archive/`](docs/archive/)
when they are kept for context.

## Development

```bash
make test
make e2e
make dev
make ci
make build-arm64
```

## Release Process

Releases are driven by Changesets and GitHub Actions:

1. Add a `.changeset/*.md` entry for each user-visible change.
2. Merge the feature PR to `master`.
3. The `release` workflow opens or updates the "Version Packages" PR.
4. Merge that Version PR to bump `package.json`, update `CHANGELOG.md`, create
   the `vX.Y.Z` tag, and publish the GitHub Release.
5. The `release-assets` workflow builds and uploads Linux/Windows binaries,
   `ftw-relay` binaries, Docker images, the Raspberry Pi image, and
   `ftw-relay-web.tar.gz`.

Do not hand-edit `CHANGELOG.md` or manually bump `package.json`; pending
release notes live in `.changeset/*.md`.

## Community

- Discord: [discord.gg/25xcBzQaux](https://discord.gg/25xcBzQaux)
- Issues: [github.com/srcfl/ftw/issues](https://github.com/srcfl/ftw/issues)

## License

Licensed under the Apache License, Version 2.0 — see [`LICENSE`](LICENSE) and
[`NOTICE`](NOTICE). Project naming guidance lives in [`TRADEMARKS.md`](TRADEMARKS.md).
Contributions are accepted under the same license via the
[Developer Certificate of Origin](CONTRIBUTING.md) (commit with `git commit -s`).

> Prior to the adoption of Apache-2.0, the project was offered under the MIT
> License. See [`NOTICE`](NOTICE) for details.
