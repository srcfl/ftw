# forty-two-watts

<img src="web/logo.jpg" alt="42W" width="120" align="right">

> Home energy management that actually works. Coordinates solar, batteries, grid, and EV chargers so your house runs on its own power.

**Status: v0.4.0-alpha** — running in production on real hardware, but the API and config format may still change. [Join the Discord](https://discord.gg/z7FxpQnk) to follow development and share feedback.

## What it does

42W is a single Go binary that runs on a Raspberry Pi (or any Linux box) and manages your home energy system in real time:

- **Self-consumption** — batteries discharge to cover household load, charge from PV surplus, grid stays near zero.
- **Smart scheduling** — an MPC planner looks 48 hours ahead using electricity prices + weather forecasts and decides when to charge, discharge, or hold.
- **EV-aware** — when your car is charging, home batteries automatically stop discharging into it (no energy round-tripping through two battery systems).
- **Multi-device** — runs multiple inverters + batteries + meters simultaneously, each with its own Lua driver. Tested with Ferroamp + Sungrow on the same site.

## Supported devices

19 Lua drivers ship today. Each is a single `.lua` file — no compilation, no toolchain, hot-reloadable on the device.

| Category | Manufacturers |
|----------|--------------|
| **Hybrid inverters** | Sungrow, Solis, Huawei, Deye, SMA, Fronius, SolarEdge, Kostal, GoodWe, Growatt, Sofar, Victron |
| **Batteries** | Ferroamp (MQTT + Modbus), Pixii |
| **Meters** | Eastron SDM630, Fronius Smart Meter |
| **EV chargers** | Easee (Cloud API) |

Adding a new device? See [Writing a driver](docs/writing-a-driver.md) or the [Claude Code recipe](docs/writing-a-driver-with-claude-code.md) to generate one from a register map.

## Quick start

### Option A — flash our Raspberry Pi 4 SD-card image (fastest)

A pre-built `42w-rpi4-arm64-vX.Y.Z.img.xz` ships with every release.
Flash it to an SD card with [Raspberry Pi Imager](https://www.raspberrypi.com/software/)
or [balenaEtcher](https://etcher.balena.io/) (both handle `.img.xz`
natively — no need to decompress first), boot the Pi, and open
`http://42w.local/`. No terminal work required.

If you don't pre-configure WiFi in Imager's advanced options, the Pi
exposes a `42w-setup` captive portal for phone-based onboarding.

Full walkthrough in [docs/rpi-image.md](docs/rpi-image.md).

### Option B — one-shot Docker installer (existing Pi or any Linux box)

Works on a freshly-installed Raspberry Pi OS (arm64) and most Debian/Ubuntu
machines. Installs Docker + compose, pulls the multi-arch image from GHCR,
and starts the container. Idempotent — re-run to upgrade.

```bash
curl -fsSL https://raw.githubusercontent.com/frahlg/forty-two-watts/master/scripts/install.sh | bash
```

Then open `http://<your-pi>:8080/setup` to run the first-time wizard.

### Option C — Home Assistant OS add-on

If you already run Home Assistant OS or HA Supervised, install
forty-two-watts as an add-on directly from the supervisor — no
separate Pi or Docker host needed. Maintained at
[erikarenhill/ha-addon-forty-two-watts](https://github.com/erikarenhill/ha-addon-forty-two-watts).

### Option D — build from source

**Prerequisites:** Go 1.25+, a Raspberry Pi (or any `linux/arm64` machine), and at least one supported inverter/battery on your LAN.

```bash
git clone https://github.com/frahlg/forty-two-watts
cd forty-two-watts

# Try it locally with simulators
make dev          # starts sim-ferroamp + sim-sungrow + the app
open http://localhost:8080

# Build for your Pi
make build-arm64
scp bin/forty-two-watts-linux-arm64 pi@<your-pi>:~/42w/
scp -r drivers/ web/ config.example.yaml pi@<your-pi>:~/42w/
```

Copy `config.example.yaml` to `config.yaml` and fill in your device IPs. The web UI at `:8080` lets you configure everything else.

## How it works

Three layers in one binary:

1. **Control loop** (every 5 s) — PI controller + slew rate + fuse guard + SoC clamps. Reads the site meter, computes battery targets, dispatches to drivers. This is the part that keeps the lights on.

2. **MPC planner** (every 15 min) — dynamic programming over a discretized SoC grid. Three strategies:
   - **Self-consumption** — never import to charge, never export from battery. Just cover your own load.
   - **Cheap charging** — charge from grid when prices are low, otherwise self-consume.
   - **Arbitrage** — full price optimization: charge cheap, discharge expensive, export when profitable.

3. **Digital twins** (every 60 s) — online machine learning models that observe your system and learn:
   - **PV twin** — learns your roof's orientation, shading, and soiling from clear-sky physics + cloud cover.
   - **Load twin** — learns your household's hourly consumption pattern + heating coefficient.
   - **Price twin** — fills in electricity prices beyond the day-ahead publication window.

## Web UI

The dashboard shows real-time power flow, battery SoC, energy totals, the planner's 48-hour schedule, and per-driver health. Everything is configurable from Settings — devices, strategies, EV charger credentials, Home Assistant integration.

## Home Assistant

Built-in MQTT autodiscovery. Enable it in Settings → Home Assistant, point it at your Mosquitto broker, and sensors + controls appear in HA automatically.

## Notifications

Get a push to your phone when something goes wrong at home. 42W can publish push notifications via [ntfy.sh](https://ntfy.sh) (or your own self-hosted ntfy server) on operator-configured events — today: a driver going offline and recovering.

Open **Settings → Notifications**:

1. Pick a topic name in the ntfy app (e.g. `forty-two-<something-random>`) and subscribe to it on your phone.
2. Enable notifications in 42W, paste the same topic name, and (optionally) a bearer access token if you're using a self-hosted ntfy with auth.
3. Toggle each event on, set its threshold (default 10 min — separate from the control-loop watchdog, which trips at 60 s for safety reasons), priority, and cooldown.
4. Customize the title/body templates if you like — they're Go `text/template` strings with access to `{{.Device}}`, `{{.Make}}`, `{{.Serial}}`, `{{.Duration}}`, `{{.DurationS}}`, `{{.EventType}}`, `{{.Timestamp}}`. Leave blank to use the built-in defaults shown in the inputs.
5. Hit **Send test notification** to verify end-to-end delivery before relying on it.

Under the hood an event bus decouples the core control loop from notifications, and the transport is selected by a strategy-pattern provider registry — adding a new provider (Pushover, Slack, …) is a drop-in Go file. Config reference: `notifications:` block in [docs/configuration.md](docs/configuration.md).

## EV charging

Configure your Easee charger in Settings → EV Charger (email + password). The driver polls the Easee Cloud API every 5 seconds. When the car charges, the dispatch clamp prevents home batteries from discharging into the car.

OCPP 1.6J Central System is also built in (port 8887) for chargers that support direct WebSocket connections.

## Architecture

```
config.yaml
    ↓
┌─────────────────────────────────────────┐
│  Lua drivers (one goroutine per device) │
│  ferroamp.lua · sungrow.lua · easee.lua │
└────────────┬────────────────────────────┘
             ↓ host.emit("meter"|"pv"|"battery"|"ev")
┌─────────────────────────────────────────┐
│  Telemetry store (Kalman-smoothed)      │
└────────────┬────────────────────────────┘
             ↓
┌─────────────────────────────────────────┐
│  Control loop (PI + dispatch + clamps)  │
│  ← MPC planner (DP, 48h horizon)       │
│  ← Digital twins (PV, load, price)     │
└────────────┬────────────────────────────┘
             ↓ driver_command(action, power_w)
┌─────────────────────────────────────────┐
│  Lua drivers → Modbus / MQTT / HTTP     │
└─────────────────────────────────────────┘
```

No cloud dependency for core operation. Everything runs locally on the Pi. Weather forecasts (met.no) and electricity prices (Elpriset Just Nu / ENTSO-E) are fetched periodically but the system degrades gracefully without them.

## Documentation

**Get started**
- [SD-card image walkthrough](docs/rpi-image.md) — flash, boot, WiFi onboarding, troubleshoot
- [Setup guide](docs/setup-guide/) — first-time wizard explained step by step
- [Reaching your home from anywhere](docs/remote-access.md) — one URL + passkey, peer-to-peer, no app
- [Configuration reference](docs/configuration.md) — every YAML key, with examples
- [Driver catalog](docs/driver-catalog.md) — supported devices and their config blocks
- [Device repository plan](docs/device-repository.md) — non-breaking driver repository rollout

**Run it**
- [Operations](docs/operations.md) — deploy, backup, upgrade, logs
- [In-app updates](docs/self-update.md) — how the `ftw-updater` sidecar works
- [Home Assistant integration](docs/ha-integration.md) — MQTT autodiscovery setup
- [Safety model](docs/safety.md) — watchdog, clamps, fuse guard, stale-meter guard

**Understand it**
- [Architecture overview](docs/architecture.md) — layers, data flow, why each piece exists
- [Site sign convention](docs/site-convention.md) — must-read before touching power-math code
- [ML models](docs/ml-models.md) — PV / load / price twins
- [MPC planner](docs/mpc-planner.md) — DP strategy details
- [Battery models](docs/battery-models.md) — ARX(1), RLS, cascade, self-tune
- [API reference](docs/api.md) — HTTP endpoints

**Build with it**
- [Writing a Lua driver](docs/writing-a-driver.md) — full walkthrough
- [Using Claude Code](docs/writing-a-driver-with-claude-code.md) — AI-assisted driver generation
- [Testing drivers live](docs/testing-drivers-live.md) — sim + Pi workflow
- [Lua host API](docs/host-api.md) — every `host.*` capability the Lua side can call

## Development

```bash
make test         # full Go test suite
make e2e          # end-to-end with simulators
make dev          # live dev with hot-reload
make build-arm64  # cross-compile for Pi
```

See [docs/development.md](docs/development.md) and [docs/testing.md](docs/testing.md) for the full dev loop, sim setup, and e2e recipes.

## Community

- **Discord**: [discord.gg/z7FxpQnk](https://discord.gg/z7FxpQnk) — discuss development, share your setup, report issues
- **Issues**: [github.com/frahlg/forty-two-watts/issues](https://github.com/frahlg/forty-two-watts/issues)

## Roadmap

- [ ] New-user onboarding flow (guided setup wizard)
- [ ] Network scanning for auto-discovery of devices
- [ ] Driver marketplace with version history and compatibility info
- [ ] EV smart charging (PV-surplus preferred, departure-time aware)
- [ ] Multi-charger load balancing with fuse coordination
- [ ] OCPP 2.0.1 support

## License

MIT
