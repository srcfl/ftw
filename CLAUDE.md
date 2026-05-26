# forty-two-watts — project orientation

Unified Home Energy Management System, written in Go with Lua drivers.
See `MIGRATION_PLAN.md` for the historical Rust→Go migration context.

## Mental model

**Site sign convention**: positive W = energy flowing INTO the site across
the grid-meter boundary. Grid import (+), PV generation (−), battery
charge (+ as load), battery discharge (−). The driver layer is the ONLY
place sign conversion happens — above it, every layer uses the site
convention. Read `docs/site-convention.md` before touching any power-math
code.

**Lua drivers**: `drivers/*.lua` loaded by `gopher-lua` are the only
driver path. Each driver file implements the lifecycle (`driver_init`,
`driver_poll`, `driver_command`, `driver_default_mode`,
`driver_cleanup`) and talks to hardware through the `host.*` capabilities
exposed by `go/internal/drivers/lua.go`. Drivers are hot-editable on the
Pi and need no build step.

**Clamping discipline**: every clamp must protect against a *quantifiable
risk*. Read `docs/clamping.md` for the seven current clamps and the
saturation-curve feedback-loop bug we shipped then fixed.

**Hardware-stable identity**: every device a driver talks to gets a
`device_id` resolved in priority order — `make:serial` (from
`host.set_make` + `host.set_sn`) > `mac:<arp-resolved>` (for TCP devices
on the same L2) > `ep:<endpoint>` (fallback). Persistent state such as
battery models is keyed on `device_id` internally, so renaming a driver
in YAML or re-adding it doesn't orphan a trained model. See
`go/internal/state/devices.go` and `go/internal/arp/arp.go`.

## Key packages

| Package | Purpose |
|---|---|
| `go/internal/config` | YAML config + validation + atomic save |
| `go/internal/state` | SQLite persistence, tiered history, long-format TS + Parquet rolloff, devices |
| `go/internal/telemetry` | DerStore with Kalman per signal + driver health + WatchdogScan |
| `go/internal/control` | PI + dispatch modes + slew + fuse guard |
| `go/internal/battery` | ARX(1) model + RLS + cascade + saturation curves |
| `go/internal/selftune` | Step-response state machine + fitter |
| `go/internal/drivers` | Lua host (`lua.go`) + Registry + capability interfaces |
| `go/internal/api` | HTTP endpoints (Go 1.22+ method mux) |
| `go/internal/configreload` | fsnotify watcher + reload dispatch |
| `go/internal/ha` | Home Assistant MQTT autodiscovery + bridge |
| `go/internal/mqtt` | paho client wrapper implementing drivers.MQTTCap |
| `go/internal/modbus` | simonvetter wrapper implementing drivers.ModbusCap |
| `go/internal/arp` | L2 MAC resolver for device identity (linux/darwin) |
| `go/internal/sunpos` | Physics-only solar zenith/azimuth (Spencer 1971) |
| `go/internal/priceforecast` | Price twin — fills beyond day-ahead publication |
| `go/internal/loadmodel` | Household load twin (bucketed + heating coefficient) |
| `go/internal/pvmodel` | PV twin (RLS over sunpos / cloud prior) |
| `go/internal/mpc` | MPC planner — DP over SoC grid, 48 h horizon |
| `go/internal/selfupdate` | GH Releases probe + trigger dispatch for the in-app updater sidecar |
| `go/internal/nova` | Opt-in federation client to Sourceful Nova Core — ES256 identity, JWT signer, HTTP client (claim + provision), clean telemetry payload + boundary adapter, MQTT publisher |
| `go/cmd/ftw-updater` | Sidecar binary — runs docker compose pull + up -d on behalf of the main service |
| `go/cmd/ftw-pair` | MCP sidecar — host side of the pair flow (`docs/ftw-pair.md`) |
| `go/cmd/ftw-connect` | Friend-side CLI for joining a pair session |
| `go/cmd/ftw-subetha` | Standalone relay server — matches two peers on a token and pipes encrypted bytes (`docs/pair-relay-deploy.md`) |
| `go/internal/subetha` | Subetha — pure-Go relay client: BIP39 token, HKDF-derived ChaCha20-Poly1305 AEAD, length-prefixed frames over TCP |
| `drivers/` | Lua drivers (`ferroamp.lua`, `sungrow.lua`, …) |
| `go/test/e2e` | Full-stack test: sims + main + drivers + HTTP |

## Building & testing

```bash
make test         # unit + integration tests
make e2e          # full-stack end-to-end test
make dev          # start sims + main app locally
make build-arm64  # cross-compile for RPi
make release      # tarballs for deploy
make verify       # pre-commit: vet + test + build (mirrors CI `go test + vet` workflow)
make verify-all   # pre-push: verify + cross-compile for linux/arm64, linux/amd64, windows
make install-hooks  # install git pre-commit + pre-push hooks (opt-in)
```

Lua drivers need no build step — `drivers/*.lua` ships verbatim with the
release tarball and is loaded on startup.

No CGo anywhere — pure Go + embedded Lua 5.1 (gopher-lua). `go build`
produces a static single-binary distribution.

## Adding a new driver

1. Copy `drivers/ferroamp.lua` as a template to `drivers/mydevice.lua`.
2. Implement `driver_init`, `driver_poll`, `driver_command`, and
   (optionally) `driver_default_mode` / `driver_cleanup`. Use the
   `host.*` helpers for I/O — full API in `go/internal/drivers/lua.go`.
3. Call `host.set_make("…")` + `host.set_sn("…")` inside `driver_init`
   as soon as you've read them off the device — that's what anchors
   `device_id` to hardware identity.
4. Use `host.emit_metric("name_unit", value)` for any scalar diagnostic
   that doesn't fit the structured pv/battery/meter emit (temperatures,
   DC voltages, MPPT currents, inverter heatsink, grid frequency). It
   lands in the long-format TS DB, queryable for life.
5. Add an entry to `config.yaml` with `lua: drivers/mydevice.lua` and
   the appropriate `capabilities:` block.
6. Driver starts on next restart (or hot-reload via the file watcher).

Full walkthrough in `docs/writing-a-driver.md`.

## Lua driver host

See the top-of-file comment in `go/internal/drivers/lua.go`. The `host`
global exposes:

- `host.log(level, msg)`, `host.millis()`, `host.set_poll_interval(ms)`
- `host.set_make(s)`, `host.set_sn(s)` — anchors device identity
- `host.emit("battery"|"pv"|"meter", {…})` — structured telemetry
- `host.emit_metric(name, value)` — arbitrary scalar diagnostics into TS DB
- `host.mqtt_sub/pub/messages`, `host.modbus_read/write/write_multi`
- `host.decode_u32_le/be`, `host.decode_i32_le/be`, `host.decode_i16`
- `host.json_encode/decode`

MQTT / Modbus / HTTP calls return an error string if the driver wasn't
granted the capability in config.

## Time-series DB (long-format)

Every `host.emit_metric` call lands in three SQLite tables defined in
`go/internal/state/store.go` and written through
`go/internal/state/store_ts.go`:

- `ts_drivers(id, name)` — interned driver names
- `ts_metrics(id, name, unit)` — interned metric names
- `ts_samples(driver_id, metric_id, ts_ms, value)` — `WITHOUT ROWID,
  STRICT`, PK clusters rows by (driver, metric, ts) so the typical
  "metric X for driver Y over range Z" query is a sequential scan.

Samples older than `RecentRetention` (14 days) roll off to daily Parquet
files under `<state.cold_dir>/YYYY/MM/DD.parquet` — see
`go/internal/state/parquet.go`. Rolloff runs hourly from
`go/cmd/forty-two-watts/main.go` (`rolloffLoop`). Parquet files are
zstd-compressed and dictionary-encoded on `driver` + `metric`, so a
year of 50 metrics is typically a few GB.

## Watchdog + safety

The control loop (`go/cmd/forty-two-watts/main.go`, the `ticker.C`
branch) runs `tel.WatchdogScan(timeout)` every cycle. Any driver whose
last successful telemetry is older than `site.watchdog_timeout_s`
(default 60 s) flips to offline and receives `DefaultMode` — which in
every driver means "drop into autonomous self-consumption". The host
also short-circuits the dispatch cycle when the configured site-meter
driver is stale, because a stale grid reading causes one battery to
charge another.

## UI / web work

**Always follow `DESIGN.md`** for any work under `web/` — colour
tokens, typography, component vocabulary, and "what NOT to do" rules.
The short version:

- Read tokens from `web/components/theme.css`. Never hard-code hex
  colours (e.g. `#6cf`, `#ffb020`); reach for `var(--accent-e)`,
  `var(--fg)`, `var(--ink-raised)`, `var(--line)`, etc.
- One amber accent only. On-accent text is near-black `#0a0a0a`,
  never white.
- Mono (`var(--mono)`) for eyebrow labels (UPPERCASE, `0.18em`
  letter-spacing) and tabular numerics; sans (`var(--sans)`) for
  prose.
- 1 px hairline borders (`var(--line)`); no drop-shadows on cards or
  modals (the only sanctioned shadow is the accent glow on 6 px status
  dots).
- Light theme support is automatic when you use the tokens — do not
  branch on `data-theme`.
- Do not reintroduce Google Fonts; fresh-Pi deploys must boot without
  WAN.

When extending an existing component, match its existing token usage
rather than adding new local colour rules. If a new component needs a
component-specific hue, follow the `--*-e` naming convention so the
light theme can flip it cleanly.

## Code conventions

- `slog` for all logging
- Explicit mutexes — no atomic tricks unless measurably needed
- SQLite queries in `internal/state/*.go`, nothing embedded elsewhere
- Driver code in Lua; the Go side only owns capabilities, not protocol logic
- Tests colocated with code, `_test.go` files
- Integration tests in `go/test/e2e/` (separate package to keep public
  and internal concerns cleanly split)

## When things look weird

- **Sign is wrong somewhere**: it's ALWAYS a bug at the driver boundary.
  Above the driver layer is always site convention.
- **Battery drifting from target**: check confidence. Below 0.5 the
  cascade bypasses the inverse model (gates on confidence intentionally).
- **History queries slow**: check `idx_hot_ts` is there; SQL uses range
  scans. `Prune()` should be running periodically to age data to warm/cold.
- **Driver hung (tick_count not advancing)**: restart the service and
  check `WatchdogScan` transitions in the logs — the loop should have
  already flipped it offline and sent `DefaultMode`. If it didn't,
  `tel.health[name].LastSuccess` never got bumped; confirm the driver
  is actually calling `host.emit`.
- **PV prediction wild**: the twin's RLS coefficients drifted on a
  run of bad weather data. `POST /api/pvmodel/reset` wipes them and
  falls back to the physics-only `sunpos` prior.
- **Battery model orphaned after rename**: it shouldn't — models are
  keyed on `device_id`, not driver name. Verify with `GET /api/devices`
  that the rename preserved the same `device_id`. If not, the driver
  isn't reporting a stable make+serial; fix it there.

## Docs for operators + devs

- `docs/site-convention.md` — sign convention (must-read)
- `docs/architecture.md` — system overview, layers, data flow (NEW)
- `docs/writing-a-driver.md` — Lua driver tutorial (NEW)
- `docs/tsdb.md` — long-format TS schema + Parquet rolloff (NEW)
- `docs/device-identity.md` — `device_id` resolution + ARP (NEW)
- `docs/safety.md` — watchdog, clamps, fuse guard, stale-meter guard (NEW)
- `docs/ml-models.md` — PV + load + price twins, MPC inputs (NEW)
- `docs/api.md` — HTTP endpoint reference (NEW)
- `docs/operations.md` — deploy, backup, upgrade, troubleshooting (NEW)
- `docs/self-update.md` — in-app update flow + ftw-updater sidecar architecture
- `docs/nova-integration.md` — opt-in federation to Sourceful Nova Core (MQTT + ES256 JWT, clean schema + legacy adapter)
- `docs/testing.md` — test strategy, sims, e2e recipe (NEW)
- `docs/configuration.md` — YAML schema reference
- `docs/battery-models.md` — ARX(1), RLS, cascade, self-tune
- `docs/clamping.md` — the safety clamps
- `docs/mpc-planner.md` — MPC strategies, confidence blending
- `docs/ml-twins.md` — older twin notes (superseded by ml-models.md)
- `docs/ha-integration.md` — Home Assistant MQTT bridge
- `docs/lua-drivers.md` — earlier Lua driver notes (superseded by writing-a-driver.md)
- `MIGRATION_PLAN.md` — historical: Rust→Go migration context (migration is complete)
