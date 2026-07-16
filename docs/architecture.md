# Architecture overview

This is the canonical map of FTW's core control and data path. Optional
subsystems such as remote access, CalDAV, notifications, Nova federation,
self-update, OCPP, and thermal/EV integrations are documented in their own
guides and connect through the API/config/state boundaries described here. For
the sign convention every number in the rest of this document obeys, see
[site-convention.md](site-convention.md) first.

## System summary

FTW is an embedded home energy management system that runs
on a Raspberry Pi or Linux host and coordinates meters, PV, batteries,
inverters, EV chargers, and V2X devices behind one site meter. A Go
control loop unifies dispatch, an MPC planner with online ML digital
twins sits on top, and per-device Lua drivers translate everything to
native Modbus, MQTT, HTTP, WebSocket, or raw TCP.

## Site sign convention

All power above the driver boundary follows a single rule:

> Positive W = power flowing **into** the site across the grid-meter
> boundary. Negative W = power flowing out (export).

Drivers are the only layer that converts from vendor conventions.
Telemetry store, control loop, MPC, API, UI, HA bridge, SQLite — all
site convention. Full treatment in [site-convention.md](site-convention.md).

If you're about to write `pv_w` math and it isn't negative, stop.

## Top-level components

### Drivers
Source: `go/internal/drivers/`, Lua driver scripts in `drivers/*.lua`.

One goroutine per driver runs the registry poll loop. Each driver has
its own Lua VM and a capability-scoped host environment (MQTT, Modbus,
HTTP, WebSocket, or TCP only if the driver's config grants it). Drivers
poll their device, call
`host.emit(...)` to push readings into the telemetry store in
site convention, and accept battery dispatch commands via
`Registry.Send`. On hang or config reload, the registry issues a
`DefaultMode` call so the device reverts to autonomous.

### Telemetry store
Source: `go/internal/telemetry/`.

In-memory only. Stores the latest raw + Kalman-smoothed reading per
(driver, DER-type) and a `DriverHealth` record per driver. Drivers
also buffer arbitrary metric samples (via `EmitMetric`) into a
per-cycle queue that the control loop drains with `FlushSamples`.
The telemetry store owns the watchdog scan — `WatchdogScan` flips
drivers with stale `LastSuccess` offline.

### State store
Source: `go/internal/state/`. Backing: SQLite (pure-Go driver).

Single file (default `state.db`). All persistent state lives here:

- `config` — k/v for mode, grid-target overrides, per-service
  serialized model state
- `events` — startup/shutdown/etc. log
- `telemetry` — last-known DER readings (crash recovery)
- `battery_models` — one JSON blob per battery, keyed by device_id
- `history_hot` / `history_warm` / `history_cold` — wide history
  snapshots at full / 15-min / daily resolution
- `ts_drivers`, `ts_metrics`, `ts_samples` — long-format TSDB
  (interned driver/metric ids, 14-day retention)
- `prices` — spot prices per (zone, slot_ts_ms)
- `forecasts` — weather + PV forecast slots
- `devices` — canonical hardware identity (make/serial/mac/endpoint)

Retention constants live in `go/internal/state/store.go`: 30 days hot,
365 days warm, and 14 days of long-format samples before parquet roll-off.

### Control loop
Source: `go/cmd/ftw/main.go` calling `go/internal/control/`.

Runs every `site.control_interval_s` seconds (default 2). Modes:

- `idle` — emit nothing
- `self_consumption` — drive the site meter toward 0 W: charge from PV
  surplus and discharge to cover local load, without intentionally exporting
  via the battery
- `peak_shaving` — cap grid import at `PeakLimitW`
- `charge` — force all batteries to max charge
- `priority` — fill one battery first, then the next
- `weighted` — split by configured weights
- `planner_self` / `planner_cheap` / `planner_arbitrage` — pull
  `grid_target_w` from the MPC plan for the current slot; fall back
  to `self_consumption` with target 0 when the plan is stale
- `planner_passive_arbitrage` — follow the planner's passive-arbitrage target
  while retaining the same stale-plan safety fallback

Outputs one `DispatchTarget` per battery driver (site convention:
+ = charge, − = discharge) after slew-rate and fuse-guard clamping.

### MPC planner
Sources: `go/internal/mpc/`, `optimizer/`.

CVXPY formulates a continuous or mixed-integer 48-hour model and HiGHS solves
it every 15 minutes by default. It consumes prices, scenario-based PV/load
forecasts, asset states, deadlines, comfort constraints, and site limits. Go
validates the complete returned trajectory before exposing it through the
existing energy-allocation contract. CLARABEL handles continuous convex
fallbacks; the former Go DP is retained only as an emergency fallback. See
[optimizer.md](optimizer.md).

### ML twins
Sources: `go/internal/pvmodel/`, `go/internal/loadmodel/`,
`go/internal/priceforecast/`.

Online learners. Each service subscribes to telemetry + forecast
inputs, updates a small model every minute, and exposes a `Predict`
function. `main.go` wires the three `Predict` functions directly
into the MPC service at startup. Serialized model state round-trips
through `state.SaveConfig`/`LoadConfig` under keys `pvmodel/state`,
`loadmodel/state`, `pricefc/state`. See [ml-models.md](ml-models.md)
for model internals.

### API + UI
Sources: `go/internal/api/`, `web/`.

HTTP REST + static UI served on port 8080 by default
(`go/internal/config/`). The UI reads live telemetry,
drives mode/target changes, and exposes the planner + model views.
Config writes go through the API to the same `config.yaml` file.

### HA bridge
Source: `go/internal/ha/`.

Optional MQTT bridge. Publishes autodiscovery for every driver plus
site-level sensors, periodically publishes state, and subscribes to
commands (set mode, set grid target, set peak limit, set EV
charging). Signs in MQTT payloads match site convention so HA charts
need no sign fiddling.

### Watchdog
In-process, owned by `telemetry.Store.WatchdogScan`, invoked once
per control tick. Any driver whose last success is
older than `site.watchdog_timeout_s` (default 60 s) is flipped
offline; the control loop then calls `reg.SendDefault` so the
device reverts to its own autonomous mode. A separate safety check
also skips the whole dispatch cycle if the site meter itself is stale:
otherwise one battery could charge off another.

## Data flow

```
          ┌────────────────────────────────────────────────────────┐
          │                     Devices                             │
          │  inverters, meters, batteries, EV/V2X chargers          │
          └──────────┬──────────────────────────┬──────────────────┘
                     │ native protocol          │ native protocol
                     ▼                          ▼
          ┌────────────────────────────────────────────────────────┐
          │                Drivers (one Lua VM each)                │
          │  translate ⇆ site convention via host.emit              │
          │  accept battery dispatch via registry.Send              │
          └──────────┬──────────────────────────┬──────────────────┘
                     │ DerReading (site conv.)  │ MetricSample queue
                     ▼                          ▼
          ┌────────────────────────────────────────────────────────┐
          │                 telemetry.Store                         │
          │   • Kalman-smoothed latest reading per (driver,type)    │
          │   • DriverHealth (watchdog, error counts)               │
          │   • pending[] metric samples (flushed per cycle)        │
          └──────────┬──────────────────────────┬──────────────────┘
                     │ Get / ReadingsByType     │ FlushSamples
                     ▼                          ▼
          ┌──────────────────────────┐   ┌──────────────────────┐
          │ control loop (2 s tick)  │   │ state.Store (SQLite) │
          │ main ticker              │   │                      │
          │ • watchdog scan          │   │ RecordSamples        │
          │ • compute dispatch       │◀──│ RecordHistory        │
          │ • slew + fuse clamp      │   │ LoadConfig (mode,    │
          │ • reg.Send per driver    │   │   grid_target, model │
          └──────────┬───────┬───────┘   │   blobs)             │
                     │       │           │ SaveBatteryModel     │
                     │       │           │ LoadForecasts/Prices │
                     │       │           └──────────┬───────────┘
                     │       │                      │
                     │       │           ┌──────────▼───────────┐
                     │       │           │ MPC planner          │
                     │       │           │ (15-min slot plan)   │
                     │       │           │ GridTargetAt(now)    │
                     │       └──────────▶│   via ctrl.PlanTarget│
                     │                   └──────────┬───────────┘
                     │                              │
                     │         ┌────────────────────┘
                     │         ▼
                     │   ┌───────────────┐       ┌────────────────┐
                     │   │ ML twins      │       │ HTTP API       │
                     │   │ pvmodel /     │──────▶│ /api/...       │
                     │   │ loadmodel /   │       │ static web UI  │
                     │   │ priceforecast │       │ :8080          │
                     │   └───────────────┘       └────────────────┘
                     │
                     ▼
          ┌──────────────────────────┐       ┌────────────────────┐
          │ HA bridge                │──────▶│ MQTT broker        │
          │ autodiscovery + state    │◀──────│ command topics     │
          └──────────────────────────┘       └────────────────────┘

  Background hourly tick:
    state.Store.RolloffToParquet ──▶ <cold_dir>/YYYY/MM/DD.parquet
                                     (ts_samples older than 14 days)
```

Cardinality: one control loop, one telemetry store, one state store,
N drivers, one MPC service (optional), three ML twin services
(optional), one API server, one optional HA bridge.

## Persistence map

| Data | Subsystem | Where (state.Store) | Key |
|---|---|---|---|
| Battery models (ARX(1) RLS) | control loop | `battery_models` table, `SaveBatteryModel` | `device_id` |
| PV twin state | `pvmodel` service | `config` table, `LoadConfig`/`SaveConfig` | `pvmodel/state` |
| Load twin state | `loadmodel` service | `config` table | `loadmodel/state` |
| Price forecast state | `priceforecast` service | `config` table | `pricefc/state` |
| FX rates | `currency` service | `config` table | `fx/ecb_rates` |
| Operator mode | API + HA bridge | `config` table | `mode` |
| Manual grid target override | API + HA bridge | `config` table | `grid_target_w` |
| Telemetry snapshots (crash recovery) | telemetry dumps | `telemetry` table | driver+type key |
| Long-format TS (14 d) | control loop (`FlushSamples`) | `ts_samples` table | `(driver_id, metric_id, ts_ms)` |
| Long-format TS (>14 d) | hourly rolloff | `<cold_dir>/YYYY/MM/DD.parquet` | UTC day |
| Wide history (30 d, control cadence) | control loop (`RecordHistory`) | `history_hot` | `ts_ms` |
| Wide history (365 d, 15 min) | `Prune` | `history_warm` | `ts_ms` |
| Wide history (daily) | `Prune` | `history_cold` | `ts_ms` |
| Devices | identity bootstrap | `devices` table | `device_id` = `make:serial` / `mac:…` / `ep:…` |
| Spot prices | `prices` service | `prices` table | `(zone, slot_ts_ms)` |
| Weather forecasts | `forecast` service | `forecasts` table | `slot_ts_ms` |
| Events | anywhere calling `RecordEvent` | `events` table | `ts_ms` |

Retention constants in `go/internal/state/store.go`: `HotRetention =
30*24h`, `WarmRetention = 365*24h`, `RecentRetention = 14*24h`
(`store_ts.go:20`).

## Configuration source of truth

Single file (`config.yaml`, default path). Example:
[`config.example.yaml`](../config.example.yaml). Semantics:
[configuration.md](configuration.md).

Hot-reload: `go/internal/configreload/watcher.go` uses `fsnotify` to
watch the config file. On change it diffs against the live `cfg`,
calls `reg.Reload` for driver add/remove/restart, and refreshes
capacity maps under the shared mutex. The API's `POST /api/config`
endpoint writes the same file atomically (`config.SaveAtomic`), so
editing via the Settings UI and editing the file by hand are
equivalent paths (see the diagram in
[configuration.md](configuration.md)).

A small set of bindings are read once at startup and not reloaded:
state file path, parquet cold dir, API port, HA broker coordinates.

## Where to read first

In this order, for someone new to the codebase:

1. [docs/site-convention.md](site-convention.md) — every number in
   the system obeys it. Nothing below makes sense otherwise.
2. This file.
3. [docs/ml-models.md](ml-models.md) for the digital twins and
   [docs/mpc-planner.md](mpc-planner.md) for the planner internals.
4. [docs/writing-a-driver.md](writing-a-driver.md) and
   [docs/host-api.md](host-api.md) for Lua drivers.
5. `go/cmd/ftw/main.go` — the process orchestrator that wires the packages
   together.
