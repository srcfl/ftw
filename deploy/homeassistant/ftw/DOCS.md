# FTW

Local-first home energy coordination — model-predictive battery dispatch,
PV / load / price planning, and hot-reloadable Lua device drivers — running
as a Home Assistant OS / Supervised add-on.

## Installation

1. Add this add-on repository to Home Assistant (**Settings → Add-ons →
   Add-on Store → ⋮ → Repositories**), then install **FTW**.
2. Start the add-on. First boot has no configuration yet, so the app comes
   up in its setup wizard.
3. Open the web UI (**Open Web UI**, or `http://<your-ha-host>:8080/setup`)
   and complete onboarding: pick your drivers, scan the LAN for devices, and
   save. The wizard writes `/data/config.yaml`.

## Configuration

Almost all configuration happens **in the web UI**, not in the add-on
Options tab. The setup wizard and the running dashboard read and write
`/data/config.yaml`, which persists across add-on updates and restarts.
There are no add-on options to set today.

### Networking

The add-on uses **host networking**, required for its core function:

- **Modbus TCP** to inverters and meters on your LAN
- **MQTT** brokers on your LAN, including the Home Assistant **Mosquitto
  broker** add-on (reachable at `core-mosquitto:1883`)
- **mDNS / broadcast discovery** (e.g. Sourceful Zap at `zap.local`)

Because of host networking, the UI is served directly on **port 8080** of the
Home Assistant host (not via Ingress). If something else already uses port
8080 on that host, the add-on will fail to bind.

### Optimizer (included)

Upstream runs FTW as several containers — the Go core, the Python/CVXPY
optimizer, an updater sidecar and (optionally) Mosquitto. A Home Assistant
add-on is single-container, so this is an **all-in-one image**: it bundles the
**core and the optimizer together** and supervises both, so you get the full
CVXPY optimizer with no extra setup. (The updater is dropped — Supervisor
handles updates — and Mosquitto is left to the HA Mosquitto add-on.)

Because the CVXPY stack (numpy/scipy) is bundled, the image is a few hundred MB
— a one-time download. If either process stops the add-on restarts as a whole.

### Adding your own Lua drivers

Drop hot-reloadable `*.lua` files into **`/data/drivers`** (via the Samba/SSH
add-ons or a file editor). They overlay the bundled drivers and survive add-on
updates. With the `share` mapping you can also keep them under `/share`.

## Data, persistence & sizing

Everything the app keeps — `config.yaml`, `state.db`, trained battery models,
and the `cold/` Parquet history — lives in the add-on's `/data` volume and is
preserved across updates.

**Please read on SD-card installs.** FTW is a time-series system; its cold
Parquet history grows roughly **a few GB per year** (50 metrics), on top of
`state.db`. That data is on the **shared Home Assistant data partition**, and
Home Assistant **includes add-on `/data` in full backups** — so an unattended
install can bloat both the partition and every backup. Recommendations:

- Prefer an **SSD / large eMMC** over a small SD card.
- Point the app's `state.cold_dir` at `/share` (or a mounted data disk) so
  history isn't on the OS partition.
- **Exclude this add-on's data** from routine Home Assistant backups unless
  you want the full history in them.

## Updates

Updates are handled by Home Assistant's Supervisor. The app's own in-app
self-update feature (the standalone Docker deploy's Update/Restart buttons) is
not part of this add-on — Supervisor owns the lifecycle here.

## Support

Issues and questions: <https://github.com/srcfl/ftw/issues>.
