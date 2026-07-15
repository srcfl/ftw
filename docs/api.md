# REST API reference

Human-readable guide to the main HTTP surface of FTW.
[`go/internal/api/api.go`](../go/internal/api/api.go) is the route source of
truth; this document should be treated as a readable reference, not a
generated route inventory.

## Conventions

- **Base URL:** `http://<host>:8080` by default; configured via `api.port`.
- **Access model:** local deployments assume a trusted LAN. Remote/owner
  access paths have their own pairing and tunnel flow. Do not expose the
  local API directly to the public internet without auth and TLS.
- **Content type:** `application/json` for both requests and responses
- **CORS:** JSON responses set permissive CORS headers for the local UI/API
  workflow.
- **WebSocket / SSE:** none. Clients poll `/api/status` and `/api/history`.
  The top comment in `go/internal/api/api.go:6` notes this explicitly
- **Static UI:** paths not matched by registered routes are served from
  `WebDir` (default `web/`).

## Site sign convention

Every power field in this document uses the site sign convention:

- `grid_w` positive = importing from grid, negative = exporting
- `pv_w` always negative = generation (leaving PV into the site)
- `bat_w` positive = charging (energy into battery), negative = discharging
- `ev_w` positive = EV charging load
- `v2x_w` positive = vehicle charging, negative = vehicle discharging
- `load_w` always positive (clamped)

See `docs/site-convention.md` for the full reasoning.

---

## Health

### GET /api/health

Aggregate driver health counters. Cheap, safe to hit frequently

**Query params:** none

**Response (200):**

```json
{
  "status": "ok",
  "drivers_ok": 2,
  "drivers_degraded": 0,
  "drivers_offline": 0
}
```

`status` is `"degraded"` if any driver is offline, else `"ok"`

Handler: `go/internal/api/api.go:163`

---

## Status + control

### GET /api/status

Live snapshot of grid, PV, batteries, dispatch, and per-driver state. This
is the main polling endpoint for the UI

**Query params:** none

**Response (200):**

```json
{
  "version": "v2.1.1",
  "mode": "self_consumption",
  "plan_stale": false,
  "grid_w": -49.5,
  "pv_w": -890.0,
  "pv_w_predicted": 0,
  "bat_w": -2462,
  "ev_w": 0,
  "v2x_w": -1000,
  "load_w": 2418,
  "load_w_predicted": 4247,
  "bat_soc": 0.749,
  "grid_target_w": 0,
  "peak_limit_w": 5000,
  "ev_charging_w": 0,
  "v2x_policy": {
    "policy": {
      "enabled": true,
      "driver_name": "ferroamp_dc2",
      "min_reserve_soc_pct": 35,
      "max_charge_w": 7000,
      "max_discharge_w": 5000,
      "export_allowed": false,
      "grid_charging_allowed": false
    },
    "drivers": {
      "ferroamp_dc2": {
        "enabled": true,
        "min_power_w": -1200,
        "max_power_w": 0,
        "max_discharge_w": 1200,
        "reasons": ["export_limited_to_import", "grid_charging_blocked"]
      }
    }
  },
  "drivers": {
    "ferroamp": {
      "status": "ok",
      "consecutive_errors": 0,
      "tick_count": 603,
      "meter_w": -49.5,
      "pv_w": -8.9,
      "bat_w": -222,
      "bat_soc": 0.78
    },
    "ferroamp_dc2": {
      "status": "ok",
      "consecutive_errors": 0,
      "tick_count": 120,
      "v2x_w": -1000,
      "v2x_vehicle_soc": 0.62,
      "v2x_dc_w": -1030,
      "v2x_dc_v": 400,
      "v2x_dc_a": -2.6,
      "v2x_connected": true,
      "v2x_status": "discharging"
    },
    "sungrow": {
      "status": "ok",
      "consecutive_errors": 0,
      "tick_count": 601,
      "bat_w": -2240,
      "bat_soc": 0.72
    }
  },
  "dispatch": [
    { "driver": "ferroamp", "target_w": -1363, "clamped": false },
    { "driver": "sungrow",  "target_w": -861,  "clamped": false }
  ],
  "energy": {
    "today": {
      "import_wh": 3420.5,
      "export_wh": 1850.2,
      "pv_wh": 6200.0,
      "bat_charged_wh": 4100.0,
      "bat_discharged_wh": 2900.0,
      "load_wh": 5670.3
    }
  }
}
```

**Side notes:**

- `pv_w` and `bat_w` are summed across every driver whose reading type is
  `DerPV` / `DerBattery`
- `bat_soc` is a capacity-weighted average across batteries, so batteries
  with more kWh pull harder
- `load_w = grid_w - bat_w - pv_w - ev_w - v2x_w`, smoothed by
  `telemetry.UpdateLoad` and clamped to ≥ 0. EV and V2X are subtracted so the
  load model tracks house demand rather than vehicle sessions
- `pv_w_predicted` is negated at emit time so it matches site-sign (negative
  for generation)
- `plan_stale` is set by the watchdog when the MPC plan is older than the
  freshness window; the dispatch loop falls back to autonomous mode when
  stale (see `docs/safety.md`)
- `v2x_policy` is a read-only policy envelope. It is present for observability
  and future planner input; the dispatch loop does not consume it yet
- Driver `status` values: `ok`, `degraded`, `offline`
  (`telemetry.DriverStatus`)
- `energy.today` is present when the state DB has at least two history
  points since midnight local time. It contains cumulative Wh counters
  computed by integrating the history points (W x dt) since midnight:
  - `import_wh` — Wh imported from grid since midnight
  - `export_wh` — Wh exported to grid since midnight
  - `pv_wh` — Wh generated by PV since midnight
  - `bat_charged_wh` — Wh charged into batteries since midnight
  - `bat_discharged_wh` — Wh discharged from batteries since midnight
  - `load_wh` — Wh consumed by house load since midnight

  All values are non-negative. The object is omitted entirely (rather
  than zeroed) when no history is available — for example, right after
  midnight before the first sample pair, or when the state DB is nil

Handler: `go/internal/api/api.go:188`

### GET /api/mode

Return the current control mode and the grid target

**Query params:** none

**Response (200):**

```json
{
  "mode": "self_consumption",
  "grid_target_w": 0
}
```

Handler: `go/internal/api/api.go:334`

### POST /api/mode

Set the control mode. Persisted to the state DB under key `mode` so it
survives restart. If the new mode is a planner mode, the MPC service is
told about it and an immediate replan is triggered

**Request body:**

```json
{ "mode": "self_consumption" }
```

Valid values (from `go/internal/control/dispatch.go:14`):

- `idle`
- `self_consumption`
- `peak_shaving`
- `charge`
- `priority`
- `weighted`
- `planner_self`
- `planner_cheap`
- `planner_passive_arbitrage`
- `planner_arbitrage`

**Response (200):**

```json
{ "status": "ok", "mode": "self_consumption" }
```

**Errors:**

- `400` `{ "error": "unknown mode: …" }` for values not in the list above
- `400` `{ "error": "…" }` on malformed JSON

Handler: `go/internal/api/api.go:343`

### POST /api/target

Set the grid target (watts). Persisted to the state DB under key
`grid_target_w`. The dispatcher applies it on its next tick

**Request body:**

```json
{ "grid_target_w": 0 }
```

Site sign: positive = aim to import, negative = aim to export, 0 = aim for
a flat meter. Usually 0 for self-consumption

**Response (200):**

```json
{ "status": "ok", "grid_target_w": 0 }
```

Handler: `go/internal/api/api.go:383`

### POST /api/peak_limit

Set the peak-shaving threshold (watts, positive). In `peak_shaving` mode
this is the ceiling the dispatcher defends. Not persisted — cleared on
restart. Set via config if you want it sticky

**Request body:**

```json
{ "peak_limit_w": 5000 }
```

**Response (200):**

```json
{ "status": "ok", "peak_limit_w": 5000 }
```

Handler: `go/internal/api/api.go:401`

### POST /api/ev_charging

Tell the dispatcher that an external load (EV charger) is about to draw
power. Used by the planner to pre-condition the battery. Not persisted

**Request body:**

```json
{ "power_w": 7400, "active": true }
```

When `active` is `false`, `ev_charging_w` is reset to 0 regardless of
`power_w`

**Response (200):**

```json
{ "status": "ok", "ev_charging_w": 7400 }
```

Handler: `go/internal/api/api.go:417`

### GET /api/v2x/policy

Return the configured V2X policy and the live allowed power envelope for each
configured or reporting V2X driver. This endpoint does not send commands.

**Query params:** none

**Response (200):**

```json
{
  "policy": {
    "enabled": true,
    "driver_name": "ferroamp_dc2",
    "vehicle_capacity_wh": 77000,
    "min_reserve_soc_pct": 35,
    "departure_target_soc_pct": 80,
    "departure_time": "07:30",
    "max_charge_w": 7000,
    "max_discharge_w": 5000,
    "export_allowed": false,
    "grid_charging_allowed": false,
    "cycle_cost_ore_kwh": 12
  },
  "drivers": {
    "ferroamp_dc2": {
      "driver": "ferroamp_dc2",
      "enabled": true,
      "min_power_w": -1200,
      "max_power_w": 0,
      "max_charge_w": 0,
      "max_discharge_w": 1200,
      "export_allowed": false,
      "grid_charging_allowed": false,
      "min_reserve_soc_pct": 35,
      "vehicle_soc": 0.72,
      "vehicle_capacity_wh": 77000,
      "reasons": ["export_limited_to_import", "grid_charging_blocked"]
    }
  }
}
```

`min_power_w` is negative discharge; `max_power_w` is positive charge. Missing
SoC, unknown connection state, offline drivers, reserve floor, or missing grid
context collapse the relevant side of the envelope to `0 W`.

Handler: `go/internal/api/api.go:2511`

### POST /api/v2x/command

Send a manual signed setpoint to a configured V2X charger driver. This is a
manual pilot endpoint; the MPC planner does not dispatch V2X automatically.

**Request body:**

```json
{ "driver": "ferroamp_dc2", "power_w": -3000 }
```

Fields:

- `driver` — optional when exactly one V2X driver is configured or reporting
  live telemetry; required when multiple V2X drivers exist
- `power_w` — signed setpoint in W. Positive charges the vehicle, negative
  discharges the vehicle
- `action` — optional. Defaults to `v2x_set_power`; `v2x_stop` sends zero

**Response (200):**

```json
{ "status": "ok", "driver": "ferroamp_dc2", "power_w": -3000 }
```

**Errors:**

- `400` `{ "error": "unsupported action" }`
- `400` `{ "error": "power_w outside allowed manual V2X range" }`
- `404` `{ "error": "no V2X driver configured" }`
- `404` `{ "error": "driver is required when multiple V2X drivers exist" }`
- `409` `{ "error": "v2x driver is not reporting live telemetry" }`
- `503` `{ "error": "driver registry not available" }`

Handler: `go/internal/api/api.go:2519`

---

## Configuration

### GET /api/config

Return the current merged config as JSON. Reads under `CfgMu.RLock()`

**Query params:** none

**Response (200):** full config shape. See `go/internal/config/config.go`
for the authoritative schema. Top-level keys include `api`, `site`,
`drivers`, `prices`, `forecast`, `mpc`, `ha`, `pvmodel`, `loadmodel`

Handler: `go/internal/api/api.go:294`

### POST /api/config

Replace the full config. The payload is validated with `config.Validate()`,
persisted atomically via `SaveConfig`, then the in-memory control state
(grid target, tolerance, slew rate, min dispatch interval) is updated
immediately. The file watcher will also pick up the change but this path
is snappier

**Request body:** full config object (same shape as `GET /api/config`)

**Response (200):**

```json
{ "status": "ok" }
```

**Errors:**

- `400` `{ "error": "invalid config: …" }` on unmarshal failure
- `400` `{ "error": "validation: …" }` if `Validate()` rejects the payload
- `500` `{ "error": "save failed: …" }` if writing the file fails

Handler: `go/internal/api/api.go:301`

---

## History + time-series

### GET /api/history

Legacy wide-snapshot query used by the live chart. One row per sample
with `grid_w`, `pv_w`, `bat_w`, `load_w`, `bat_soc` flattened, plus any
extra fields that were captured as a JSON blob

**Query params:**

- `range` — one of `5m`, `15m`, `1h`, `6h`, `24h`, `3d` (default `5m`)
- `points` — max number of points to downsample to (default `200`)

**Response (200):**

```json
{
  "range": "1h",
  "items": [
    {
      "ts": 1744579200000,
      "grid_w": -49.5,
      "pv_w": -890.0,
      "bat_w": -2462,
      "load_w": 2418,
      "bat_soc": 0.749
    }
  ]
}
```

**Side notes:**

- Timestamps are Unix milliseconds
- Source is the `history` table in the state SQLite DB; rows are written by
  the dispatcher
- Unknown `range` values silently fall back to `5m`

Handler: `go/internal/api/api.go:575`

### GET /api/series

Long-format time-series query — one metric per driver. Used by the metric
browser UI and for ML training data exports. Backed by the long-format
SQLite TSDB; see `docs/tsdb.md`

**Query params (required):**

- `driver` — e.g. `ferroamp`, `sungrow`
- `metric` — e.g. `battery_w`, `meter_w`, `pv_w`, `battery_soc`

**Query params (optional):**

- `range` — `5m|15m|1h|6h|24h|3d` (default `1h`)
- `points` — downsample cap; `0` / omitted = no cap

**Response (200):**

```json
{
  "driver": "ferroamp",
  "metric": "battery_w",
  "range": "1h",
  "points": [
    { "ts": 1744579200000, "v": -222.0 },
    { "ts": 1744579260000, "v": -231.5 }
  ]
}
```

**Errors:**

- `400` `{ "error": "driver and metric are required" }`
- `500` `{ "error": "…" }` on DB read failure

Handler: `go/internal/api/api.go:724`

### GET /api/series/catalog

List every driver name and every metric name that has at least one sample
in the TSDB. Used by the metric browser dropdowns

**Query params:** none

**Response (200):**

```json
{
  "drivers": ["ferroamp", "sungrow"],
  "metrics": ["battery_soc", "battery_w", "meter_w", "pv_w"]
}
```

Both lists are sorted alphabetically

Handler: `go/internal/api/api.go:756`

---

## Devices + drivers

### GET /api/drivers

Per-driver health map. Lighter than `/api/status` when you only want the
health surface

**Query params:** none

**Response (200):** map keyed by driver name

```json
{
  "ferroamp": {
    "Name": "ferroamp",
    "Status": 0,
    "LastSuccess": "2026-04-14T12:00:00Z",
    "ConsecutiveErrors": 0,
    "LastError": "",
    "TickCount": 603
  }
}
```

**Side notes:** `Status` is an integer enum — `0=ok`, `1=degraded`,
`2=offline`. The string form is only emitted by `/api/status`

Handler: `go/internal/api/api.go:438`

### GET /api/devices

Every registered device with its hardware-stable identity (SN → MAC →
endpoint fallback chain). UIs surface this in driver cards ("SN: ABC")
and in Settings → Devices. See `docs/device-identity.md`

**Query params:** none

**Response (200):**

```json
{
  "devices": [
    {
      "device_id": "ferroamp-EHAB-1234567",
      "driver_name": "ferroamp",
      "make": "ferroamp",
      "serial": "EHAB-1234567",
      "mac": "aa:bb:cc:dd:ee:ff",
      "endpoint": "mqtt://192.168.1.10:1883",
      "first_seen_ms": 1744400000000,
      "last_seen_ms": 1744579200000
    }
  ]
}
```

`device_id` is derived from `make + serial + mac + endpoint` by
`state.ResolveDeviceID`. Once bound it never changes — even if the driver
is renamed in config

Handler: `go/internal/api/api.go:777`

### GET /api/drivers/catalog

Enumerate the `.lua` driver files in `<config-dir>/drivers/`, parsed from
each file's `DRIVER={…}` metadata table. Used by the Settings UI to offer
an "Add from catalog" dropdown

**Query params:** none

**Response (200):**

```json
{
  "path": "drivers",
  "entries": [
    {
      "path": "drivers/ferroamp.lua",
      "filename": "ferroamp.lua",
      "id": "ferroamp",
      "name": "Ferroamp EnergyHub",
      "manufacturer": "Ferroamp",
      "version": "1.0.0",
      "protocols": ["mqtt"],
      "capabilities": ["meter", "pv", "battery"],
      "description": "EnergyHub XL/S via MQTT",
      "homepage": "https://ferroamp.com"
    }
  ]
}
```

**Side notes:**

- If the catalog directory cannot be scanned, the response still returns
  `200` with `entries: []` and an `error` field
- Files without a `DRIVER` block are still listed — `id`/`name` just
  come back empty so the operator can see the file exists

Handler: `go/internal/api/api.go:445`

---

### POST /api/drivers/fingerprint

Probe an open endpoint discovered by a network scan and ask every catalog
driver that speaks the endpoint's protocol whether the device is one of
its own (via each driver's `driver_fingerprint` hook — see
`docs/writing-a-driver.md` §2.3). The probe is passive: it never runs
`driver_init`/`driver_cleanup`, so it cannot reconfigure the device.
Modbus (port 502) and HTTP (port 80) are fingerprintable.

**Body:**

```json
{ "host": "10.0.0.7", "port": 502, "protocol": "modbus", "unit_id": 1 }
```

`protocol` is inferred from the port when omitted (502 → `modbus`,
80 → `http`). `unit_id` is Modbus-only and defaults to `1`.

**Response (200):**

```json
{
  "host": "10.0.0.7",
  "port": 502,
  "protocol": "modbus",
  "unit_id": 1,
  "matches": [
    { "driver": "solaredge.lua", "name": "SolarEdge inverter + meter",
      "match": "match", "make": "SolarEdge", "serial": "7E0A1B2C",
      "confidence": 0.97 }
  ],
  "tried": [
    { "driver": "solaredge.lua", "name": "SolarEdge inverter + meter", "match": "match", "make": "SolarEdge", "serial": "7E0A1B2C", "confidence": 0.97 },
    { "driver": "sungrow.lua", "name": "Sungrow SH Hybrid Inverter", "match": "no_match" },
    { "driver": "ferroamp_modbus.lua", "name": "Ferroamp EnergyHub (Modbus)", "match": "unknown" }
  ],
  "elapsed_ms": 412
}
```

`matches` are the confirmed hits (`match`), best confidence first. `tried`
lists every candidate's verdict — `match` / `no_match` / `unknown` — for
transparency; a driver with no `driver_fingerprint` hook reports `unknown`.

**Side notes:**

- `400` for a missing host/port, an uninferable port, or an unsupported
  protocol (only `modbus`/`http`); `503` for a `modbus` request when no
  Modbus factory is wired. HTTP needs no factory.
- A candidate whose connection fails (host unreachable, wrong unit id,
  refused HTTP) degrades to an `unknown` verdict carrying the error — the
  request still returns `200`.

Handler: `go/internal/api/api_drivers_fingerprint.go`

---

### GET /api/scan

Probe the local `/24` subnets for open ports on common energy-protocol
ports (Modbus 502, MQTT 1883, HTTP 80). Used by Settings → Scan and the
bootstrap wizard.

**Query params:**

- `fingerprint` — when set (e.g. `?fingerprint=1`), each discovered Modbus
  and HTTP host is additionally run through `POST /api/drivers/fingerprint`'s
  sweep and annotated with a `matches` array (same shape as above). Opt-in
  because it opens a connection per candidate driver and is materially
  slower than a bare port scan. Without the flag the response is unchanged.

**Response (200):**

```json
[
  { "ip": "10.0.0.7", "port": 502, "protocol": "modbus", "latency_ms": 3,
    "matches": [ { "driver": "solaredge.lua", "match": "match", "make": "SolarEdge", "confidence": 0.97 } ] },
  { "ip": "10.0.0.9", "port": 1883, "protocol": "mqtt", "latency_ms": 1 }
]
```

`matches` appears only with `?fingerprint=1` and only on Modbus hosts that
a driver recognised; non-Modbus hosts pass through unannotated.

Handler: `go/internal/api/api.go` (`handleScan`)

---

## MPC planner

### GET /api/mpc/plan

Return the most recent MPC plan plus metadata about when and why it was
generated. See `docs/mpc-planner.md`

**Query params:** none

**Response (200), planner enabled:**

```json
{
  "enabled": true,
  "plan": {
    "generated_at_ms": 1744579200000,
    "mode": "self_consumption",
    "horizon_slots": 96,
    "capacity_wh": 20000,
    "initial_soc_pct": 74.9,
    "total_cost_ore": -120.5,
    "solver": {
      "engine": "cvxpy",
      "backend": "highs",
      "status": "optimal",
      "formulation": "milp",
      "solve_ms": 418.2,
      "mip_gap": 0.001,
      "scenario_count": 3
    },
    "dp_shadow": {
      "total_cost_ore": -98.2,
      "active_minus_shadow_ore": -22.3,
      "forecast_basis": "downside-pv fallback input",
      "mean_abs_battery_delta_w": 418.5,
      "max_abs_battery_delta_w": 2400,
      "direction_disagreements": 7,
      "compared_slots": 193,
      "first_action": {"battery_w": 0, "grid_w": -620, "soc_pct": 74.9}
    },
    "actions": [
      {
        "slot_start_ms": 1744579200000,
        "slot_len_min": 15,
        "price_ore": 42.1,
        "pv_w": -800,
        "load_w": 2400,
        "battery_w": -1600,
        "grid_w": 0,
        "soc_pct": 73.2,
        "cost_ore": 0,
        "confidence": 0.85,
        "reason": "cover load from battery",
        "pv_limit_w": 0,
        "storage_power_w": {"battery-east": -600, "battery-west": -1000},
        "storage_energy_wh": {"battery-east": 7300, "battery-west": 7340}
      }
    ]
  },
  "meta": {
    "last_replan_ms": 1744579100000,
    "last_replan_reason": "schedule"
  }
}
```

**Response (200), planner disabled:**

```json
{ "enabled": false }
```

When enabled but no plan has been computed yet: `plan: null`, `meta` still
populated

Handler: `go/internal/api/api.go:692`

### POST /api/mpc/replan

Force an immediate replan. Returns the resulting plan

**Request body:** ignored (none required)

**Response (200):**

```json
{ "enabled": true, "plan": { "...": "same Plan shape as GET /api/mpc/plan" } }
```

**Errors:**

- `400` `{ "error": "mpc disabled" }` if the MPC service is nil

Handler: `go/internal/api/api.go:710`

---

## ML twins

### GET /api/pvmodel

PV digital-twin self-learner status. See `docs/ml-models.md`.

**Query params:** none

**Response (200), enabled:**

```json
{
  "enabled": true,
  "samples": 4320,
  "mae_w": 85.2,
  "rated_w": 8500,
  "quality": 0.84,
  "last_ms": 1744579200000,
  "forgetting": 0.999,
  "beta": [0.8, 0.1, -0.05]
}
```

**Response (200), disabled:** `{ "enabled": false }`

Handler: `go/internal/api/api.go:801`

### POST /api/pvmodel/reset

Clear the PV twin's learned weights back to baseline. Useful after
panel changes

**Request body:** ignored

**Response (200):**

```json
{ "status": "reset" }
```

**Errors:** `400` `{ "error": "pvmodel disabled" }`

Handler: `go/internal/api/api.go:819`

### GET /api/loadmodel

Load digital-twin self-learner status. Buckets are per-hour-of-week;
`buckets_warm` is the count where `samples ≥ MinTrustSamples`

**Query params:** none

**Response (200), enabled:**

```json
{
  "enabled": true,
  "profile": "home",
  "samples": 10800,
  "mae_w": 140.0,
  "peak_w": 6800,
  "quality": 0.84,
  "last_ms": 1744579200000,
  "heating_w_per_degc": 45.0,
  "buckets_warm": 98,
  "buckets_total": 168,
  "profiles": {
    "home": { "samples": 10800, "quality": 0.84 },
    "away": { "samples": 1440, "quality": 0.42 }
  }
}
```

**Response (200), disabled:** `{ "enabled": false }`

Handler: `go/internal/api/api_loadmodel.go`

### POST /api/loadmodel/profile

Switch the active load profile. The active profile is the one that trains
from live telemetry and feeds MPC predictions. The choice is persisted in
the state DB and triggers an immediate MPC replan when MPC is enabled.

**Request body:**

```json
{ "profile": "away" }
```

Allowed values: `home`, `away`.

**Response (200):** `{ "status": "ok", "profile": "away" }`

**Errors:** `400` `{ "error": "unknown load profile: …" }`

Handler: `go/internal/api/api_loadmodel.go`

### POST /api/loadmodel/reset

Clear the active load profile while preserving the configured
`heating_w_per_degc`. Same semantics as `/api/pvmodel/reset`.

**Response (200):** `{ "status": "reset", "profile": "away" }`

**Errors:** `400` `{ "error": "loadmodel disabled" }`

Handler: `go/internal/api/api_loadmodel.go`

### GET /api/research/load/dump

Download an explicit opt-in load-research bundle as a gzipped tarball.
The bundle contains no raw config, logs, hostnames, driver names, device
serials, or endpoints. It is intended for offline model analysis across
real installations.

**Query params:**

- `days` — history window in days, `1..365`, default `120`.

**Contents:**

- `manifest.json` — format version, generated timestamp, stable random
  research `site_id`, window, bucket size, privacy statement, and load
  sign convention.
- `site.json` — non-identifying site shape: fuse size, price zone,
  provider names, total battery capacity, PV rated W, loadpoint count,
  EV presence, and heating coefficient.
- `timeseries_15m.csv` — 15-minute aggregates:
  `grid_w,pv_w,bat_w,ev_w,house_load_w,recorded_load_w,bat_soc,temp_c,cloud_pct,total_ore_kwh,spot_ore_kwh`.
- `loadmodel_state.json` — current load twin state, or `{}` if disabled.

`house_load_w = max(grid_w - pv_w - bat_w - ev_w, 0)` using the site sign
convention. `recorded_load_w` is included separately so older bundles can
be audited across changes in history semantics.

**Response (200):** `application/gzip`

**Errors:** `400` invalid `days`, `503` state store unavailable.

---

## Battery models + self-tune

### GET /api/battery_models

Inspect the per-battery first-order response models. Values come directly
from `battery.Model` — see `docs/battery-models.md` for field semantics

**Query params:** none

**Response (200):** map keyed by battery/driver name

```json
{
  "ferroamp": {
    "tau_s": 3.2,
    "gain": 0.98,
    "deadband_w": 40,
    "n_samples": 1840,
    "confidence": 0.91,
    "health_score": 0.97,
    "health_drift_per_day": 0.0005,
    "baseline_gain": 1.0,
    "baseline_tau_s": 3.5,
    "last_calibrated_ts_ms": 1744380000000,
    "last_updated_ts_ms": 1744579100000,
    "max_charge_curve": [[0.0, 5000], [0.8, 3000], [1.0, 500]],
    "max_discharge_curve": [[0.0, 500], [0.2, 3000], [1.0, 5000]],
    "a": 0.72,
    "b": 0.28
  }
}
```

Handler: `go/internal/api/api.go:475`

### POST /api/battery_models/reset

Replace one or all battery models with a fresh `battery.New(name)`.
Persisted back to the state DB

**Request body (one of):**

```json
{ "battery": "ferroamp" }
```

```json
{ "all": true }
```

**Response (200):**

```json
{ "reset": ["ferroamp"] }
```

**Errors:**

- `400` `{ "error": "provide 'battery' or 'all'" }`
- `404` `{ "error": "battery not found: …" }`

Handler: `go/internal/api/api.go:501`

### POST /api/self_tune/start

Begin a self-tune sweep across the given batteries. Holds `ModelsMu` for
the duration of the kickoff. Fails with `409` if a sweep is already
running

**Request body:**

```json
{ "batteries": ["ferroamp", "sungrow"] }
```

**Response (200):**

```json
{ "status": "started", "batteries": ["ferroamp", "sungrow"] }
```

**Errors:**

- `400` malformed JSON
- `409` `{ "error": "already running" }` or similar from the coordinator

Handler: `go/internal/api/api.go:542`

### GET /api/self_tune/status

Return the self-tune coordinator's current state

**Query params:** none

**Response (200):**

```json
{
  "active": true,
  "battery_index": 1,
  "battery_total": 2,
  "current_battery": "sungrow",
  "current_step": "stabilize",
  "step_elapsed_s": 12.0,
  "total_elapsed_s": 450.2,
  "before": {
    "ferroamp": { "tau_s": 3.5, "gain": 1.0 }
  },
  "after": {
    "ferroamp": { "tau_s": 3.2, "gain": 0.98 }
  },
  "last_error": ""
}
```

`before` / `after` carry `ModelSnapshot` maps captured at sweep start and
step completion. Full field list in
`go/internal/selftune/selftune.go:344`

Handler: `go/internal/api/api.go:561`

### POST /api/self_tune/cancel

Cancel any in-flight sweep. Idempotent — cancelling when idle is a no-op

**Request body:** ignored

**Response (200):**

```json
{ "status": "cancelled" }
```

Handler: `go/internal/api/api.go:565`

---

## Home Assistant

### GET /api/ha/status

Live status of the Home Assistant MQTT bridge. Used by the Settings UI
to show a connection indicator instead of trusting "it's saved"

**Query params:** none

**Response (200), bridge enabled:**

```json
{
  "enabled": true,
  "connected": true,
  "broker": "192.168.1.20:1883",
  "last_publish_ms": 1744579200000,
  "sensors_announced": 24
}
```

**Response (200), bridge disabled:**

```json
{ "enabled": false }
```

Handler: `go/internal/api/api.go:459`

---

## Forecast + prices

### GET /api/prices

Spot prices for the configured zone. Slot length varies by source (15 min
for NordPool/elprisetjustnu since late 2025, mostly 60 min for ENTSOE)

**Query params (all optional):**

- `range` — `5m|15m|1h|6h|24h|3d`; when present, starts at `now` and
  ends `now + range`
- `since_ms` — explicit start in Unix ms
- `until_ms` — explicit end in Unix ms

Defaults: `since = now - 1h`, `until = now + 48h`

**Response (200), enabled:**

```json
{
  "zone": "SE3",
  "enabled": true,
  "items": [
    {
      "zone": "SE3",
      "slot_ts_ms": 1744579200000,
      "slot_len_min": 15,
      "spot_ore_kwh": 42.1,
      "total_ore_kwh": 98.7,
      "source": "elprisetjustnu",
      "fetched_at_ms": 1744570000000
    }
  ]
}
```

**Response (200), disabled:**

```json
{ "enabled": false, "items": [] }
```

`total_ore_kwh` = spot plus whatever transmission/retailer markup is in
the config; `spot_ore_kwh` is the raw day-ahead price

Handler: `go/internal/api/api.go:634`

### GET /api/forecast

Weather-derived PV forecast rows

**Query params:**

- `range` — `5m|15m|1h|6h|24h|3d`; when present, starts at `now`

Default: `since = now - 1h`, `until = now + 48h`

**Response (200), enabled:**

```json
{
  "enabled": true,
  "items": [
    {
      "slot_ts_ms": 1744579200000,
      "slot_len_min": 60,
      "cloud_cover_pct": 35.0,
      "temp_c": 8.2,
      "solar_wm2": 420.0,
      "pv_w_estimated": 3400.0,
      "source": "open-meteo",
      "fetched_at_ms": 1744570000000
    }
  ]
}
```

`cloud_cover_pct`, `temp_c`, `solar_wm2`, `pv_w_estimated` are nullable —
they'll be omitted from the row when the source didn't provide them

**Response (200), disabled:**

```json
{ "enabled": false, "items": [] }
```

Handler: `go/internal/api/api.go:670`

## Self-update

In-app version check and pull+restart dispatch to the `ftw-updater`
sidecar. See [self-update.md](self-update.md) for the end-to-end
architecture (UDS, shared tmpfs state.json, skip semantics).

### GET /api/version/check

Cached GitHub Releases probe.

**Query params:**

- `force=1` — bypass the 3 h cache and hit GitHub now

**Response (200):**

```json
{
  "current": "v1.2.3",
  "channel": "stable",
  "channels": ["stable", "beta", "edge"],
  "latest": "v1.3.0",
  "update_available": true,
  "skipped": false,
  "skipped_version": "",
  "published_at": "2026-04-17T10:00:00Z",
  "release_notes_url": "https://github.com/srcfl/ftw/releases/tag/v1.3.0",
  "checked_at": "2026-04-18T08:20:04Z"
}
```

`skipped` is true only when `skipped_version == latest` — a newer
release resurfaces automatically without requiring an explicit unskip.

503 when the self-update service is disabled (`SelfUpdate == nil` in
Deps). 502 when `force=1` and GitHub is unreachable; the cached view is
still included in the body.

### POST /api/version/channel

Persist the selected release stream in `state.db` without pulling an
image. The cached target is cleared and the UI follows with a forced
version check. An actual update remains a separate, snapshot-protected
operation.

**Request body:** `{"channel":"beta"}` where channel is `stable`,
`beta`, or `edge`.

Returns the cleared version-check state with the newly selected channel.
Returns 409 if an update is already in progress.

### POST /api/version/skip

Persist a dismissed version in `state.db` `config` KV under
`update.skipped_version`.

**Request body:** `{"version":"v1.3.0"}` — empty string is rejected (400).

### POST /api/version/unskip

Clear the persisted skip. Called from the UI's "Check for updates"
action so a previously-hidden version resurfaces.

### POST /api/version/update

Signal the sidecar to `docker compose pull FTW` +
`docker compose up -d FTW`. Returns 202 as soon as the
sidecar acknowledges; the UI polls `/api/version/update/status` for
progress. 502 if the sidecar socket isn't reachable.

### POST /api/version/restart

Same as Update but with `--force-recreate` so the main service restarts
even when the image digest hasn't changed. Exists to let operators
exercise the full update flow end-to-end before cutting a release.

### GET /api/version/update/status

Pass-through of the sidecar's `state.json` from the shared tmpfs volume.
Polled every 2 s by the UI during the countdown.

**Response (200):**

```json
{
  "state": "pulling",
  "action": "update",
  "target": "v1.3.0",
  "started_at": "2026-04-18T08:20:10Z",
  "updated_at": "2026-04-18T08:20:12Z",
  "message": ""
}
```

`state` is one of `idle | pulling | restarting | done | failed`. A
stale in-flight state (no heartbeat for 5 min) is surfaced as `failed`
so the UI overlay unblocks.

## Loadpoints

### GET /api/loadpoints

Returns every configured loadpoint plus its current planner state. Used
by the UI's EV-charger modal and any external integration that wants to
display or steer EV charging.

**Response (200):**

```json
{
  "enabled": true,
  "loadpoints": [
    {
      "id": "garage",
      "driver_name": "easee-cloud",
      "plugged_in": true,
      "current_soc_pct": 38.9,
      "current_power_w": 4830,
      "delivered_wh_session": 6482,
      "target_soc_pct": 100,
      "target_time": "2026-05-02T16:00:00Z",
      "soc_source": "vehicle",
      "vehicle_soc_pct": 54,
      "vehicle_charge_limit_pct": 70,
      "vehicle_charging_state": "Charging",
      "vehicle_driver": "tesla-vehicle",
      "min_charge_w": 1380,
      "max_charge_w": 11000,
      "allowed_steps_w": [0, 1380, 1610, 1840, 2070, 2300, 2530, 2760, 4140, 4830, 5520, 6210, 6900, 7400, 7590, 8280, 11000],
      "surplus_only": false,
      "updated_at_ms": 1777717655127
    }
  ]
}
```

`surplus_only` is always rendered (no `omitempty`) so a polling client
can distinguish "explicitly off" from "field absent because the server
is too old to know about the flag".

### POST /api/loadpoints/{id}/target

Sets user intent for an EV loadpoint: target SoC, deadline, and/or the
surplus-only flag. Triggers an MPC replan so the new state lands in the
schedule fast.

**Body — all fields optional (pointers in the wire schema):**

```json
{
  "soc_pct": 100,
  "target_time_ms": 1777734000000,
  "surplus_only": true
}
```

| Field | Behaviour |
|---|---|
| `soc_pct` (omit / null) | preserves existing target SoC |
| `soc_pct: 0` | clears the target |
| `target_time_ms` (omit / null) | preserves existing deadline |
| `target_time_ms: 0` | clears the deadline (charge opportunistically) |
| `surplus_only: true` | EV charges only from PV surplus — the optimizer refuses any plan that would import grid for this loadpoint, dispatch live-clamps to PV-minus-load, and the charger is held on 3-phase steps to avoid contactor-wearing phase swaps |
| `surplus_only: false` | clears the flag (default) |
| all three omitted | 400 — handler refuses no-op requests |

The "preserves existing" semantics matter because clients that only want
to toggle `surplus_only` (e.g. the EV-charger modal's checkbox) would
otherwise zero the SoC + deadline by accidentally sending zeros.

**Response (200):** `{"ok": true}`
**Errors:** `404` for unknown loadpoint id, `400` for malformed body or empty patch.

### POST /api/loadpoints/{id}/soc

Operator correction for inferred vehicle SoC. Re-anchors the
loadpoint manager's plug-in SoC so `current_soc_pct` matches what the
operator just read off the car. Only valid while the loadpoint is
plugged in. `404` for unknown id.

**Body:** `{"soc_pct": 51}`
