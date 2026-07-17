# Configuration

FTW is configured by one YAML file, normally `config.yaml`.
The Settings UI reads and writes the same file through the REST API, so
editing the file and saving through the UI are equivalent paths.

The concrete schema lives in [`config.example.yaml`](../config.example.yaml)
and [`go/internal/config/config.go`](../go/internal/config/config.go). This
document explains the operator-facing fields and hot-reload behavior.

## Source of Truth

```
config.yaml
   |
   +-- edited by operator and watched by fsnotify
   |
   +-- written by POST /api/config from the Settings UI
   |
   v
go/internal/config.Load + Validate
   |
   v
runtime reload: drivers, control settings, planner inputs
```

The API writes changes atomically via `config.SaveAtomic`: write a temporary
file, then rename it over `config.yaml`. If a file edit is invalid, the
running process keeps the old config.

## `site`

Control-loop and safety defaults:

```yaml
site:
  name: "Home"
  control_interval_s: 2
  grid_target_w: 0
  grid_tolerance_w: 42
  watchdog_timeout_s: 60
  smoothing_alpha: 0.3
  gain: 0.5
  slew_rate_w: 3000
  slew_enabled: true
  min_dispatch_interval_s: 2
  troubleshooting_mode: false     # extra incident diagnostics; no control changes
```

`grid_target_w` is the PI setpoint in site convention. `0` means
self-consumption around the site meter. `slew_rate_w` is a soft per-cycle
ramp ceiling; per-driver power caps and the fuse guard remain the hard
dispatch constraints.

### Global Troubleshooting Mode (`site.troubleshooting_mode`)

Operators should enable this from **Settings → Control → Troubleshooting
mode** while diagnosing a live system issue. The setting is saved through the
same config API as the rest of Settings; do not ask users to edit YAML for an
incident.

It does not change planner, dispatch, clamp, or driver command behavior. It
adds one dispatch-decision log line per control cycle, exposes the flag in
`/api/status`, and passes a reserved `_troubleshooting_mode` flag to Lua
drivers so driver-specific diagnostics can emit richer status/readback data.

Turn it off in Settings after the incident; logs and long-format metrics become
noisier while it is enabled.

## `fuse`

```yaml
fuse:
  max_amps: 16
  phases: 3
  voltage: 230
```

The fuse guard derives site power from `max_amps * phases * voltage` and
scales charge/discharge targets that would otherwise breach the shared
breaker.

## `drivers`

Every driver is a Lua file with a capability block that grants only the
resources the sandbox may use:

```yaml
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    battery_capacity_wh: 15200
    max_charge_w: 10000
    max_discharge_w: 10000
    inverter_group: ferroamp
    capabilities:
      mqtt:
        host: 192.168.1.153
        port: 1883
        username: extapi
        password: ferroampExtApi

  - name: sungrow
    lua: drivers/sungrow.lua
    battery_capacity_wh: 9600
    capabilities:
      modbus:
        host: 192.168.1.10
        port: 502
        unit_id: 1
```

Rules:

- `name` is the stable API/UI key for this configured instance.
- `lua` is required; Lua is the only current driver runtime.
- Exactly one configured driver must have `is_site_meter: true`.
- A driver must have at least one of `mqtt`, `modbus`, `http`,
  `websocket`, or `tcp` under `capabilities`.
- `battery_capacity_wh` enables the driver as a controllable battery.
  Battery emits from drivers without capacity are ignored for dispatch.
- `max_charge_w` and `max_discharge_w` are optional per-driver caps.
  When unset or zero, the dispatcher uses its conservative default.
- `inverter_group` keeps PV and battery flows local to a physical inverter
  group when several inverters share the same site.

Older configs with top-level `mqtt:` or `modbus:` under a driver are still
accepted for compatibility. New configs should use `capabilities:`.

## Driver-Specific `config`

Each driver may read its own nested config block:

```yaml
drivers:
  - name: ferroamp
    lua: drivers/ferroamp.lua
    supports_pv_curtail: true
    config:
      charge_ceil_soc: 0.95
      discharge_floor_soc: 0.15
      pplim_release_w: 15000
```

Driver-specific fields are parsed by the Lua driver, not by the generic
config schema. See the driver source and
[`docs/driver-catalog.md`](driver-catalog.md) for expected keys.

The same `pplim_release_w` value (when set > 0) also arms a self-
healing watchdog in the Ferroamp driver. If the SSO reports the
sticky-pplim signature — DC bus voltage above 200 V, zero PV current,
no fault, relay closed — continuously for ten minutes, the driver
auto-publishes `pplim arg=<pplim_release_w>` to release the lock.
A five-minute cooldown prevents command-spam if the recovery doesn't
take. Operators who leave `pplim_release_w` unset see a per-incident
warning log instead — no MQTT publish, because we don't have a safe
release value to send.

The `stuck_pv_recovery_count` metric tracks how many auto-recoveries
the driver has issued since startup; alert on any non-zero rate to
catch a chronic sticky-pplim condition that needs operator attention.

### Pixii diagnostics in troubleshooting mode

Pixii PowerShaper exposes standard SunSpec battery status points near
the SoC registers. Enable **Settings → Control → Troubleshooting mode** when
a site reports symptoms like "manual charge/discharge does nothing", "Pixii
charges to 100 %", or the Pixii UI says batteries are calibrating/testing.

```yaml
- name: pixii
  lua: drivers/pixii.lua
  is_site_meter: true
  battery_capacity_wh: 20000
  capabilities:
    modbus:
      host: 192.168.1.50
      port: 502
      unit_id: 1
```

When global troubleshooting is enabled, the driver emits extra long-format
metrics:

- `battery_charge_status_code` — SunSpec 802 `ChaSt`; `7` means
  `TESTING`, which is the best standard signal for Pixii
  calibration/testing.
- `battery_control_mode_code` — `0` remote, `1` local.
- `battery_state_code`, `battery_vendor_state_code`,
  `battery_event1_bits` — raw battery state/event diagnostics.
- `pixii_setpoint_ems_w` / `pixii_setpoint_native_w` — readback of
  Pixii's active setpoint registers after commands.
- `pixii_last_command_*` — last command sent by FTW and whether the
  Modbus write succeeded.

The driver also logs status transitions as `Pixii: status ...`. If
`charge_status=testing`, assume Pixii may be calibrating/testing and may
ignore external setpoints until the battery exits that state. The legacy
per-driver `config.troubleshooting_mode: true` flag still works, but the
preferred operator path is the global site flag.

## `api`

```yaml
api:
  port: 8080
```

The API port is bound at process startup and requires restart to change.

## `homeassistant`

```yaml
homeassistant:
  enabled: false
  broker: 192.168.1.1
  port: 1883
  username: homeems
  password: homeems
  publish_interval_s: 5
```

The HA bridge uses MQTT autodiscovery. Broker changes require restart
because the bridge connection is built at startup.

## `caldav`

FTW can host an in-process CalDAV server and turn calendar events into planner
intents while publishing EV-session history and planned energy windows:

```yaml
caldav:
  enabled: false
  listen: ":5232"
  url: http://localhost:5232
  username: ftw
  manage_credentials: true
  calendar_path: /ftw/energy/
  poll_interval_s: 300
  horizon_days: 7
  ev_default_target_soc_pct: 80
  evse_history: true
  history_path: /ftw/history/
  publish_plan: true
  plan_path: /ftw/plan/
  plan_publish_interval_s: 900
```

`manage_credentials` defaults on when CalDAV is enabled. FTW generates the
password, stores it in `state.db` rather than YAML, and shows it in Settings →
Calendar. `calendar_path`, `history_path`, and `plan_path` must remain distinct
so generated events are never re-read as inbound constraints. Keyword lists and
the default EV loadpoint can be localized/selected; see
[`caldav-integration.md`](caldav-integration.md).

Enabling/disabling the server or changing `listen`, credentials, or collection
layout requires restart. Planner inputs derived by the already-running calendar
service update on its polling interval.

## `state`

Persistent state uses SQLite:

```yaml
state:
  path: state.db
  cold_dir: cold
  cold_retention_days: 0 # 0 = keep cold Parquet forever (default)
```

The SQLite DB stores config overrides, events, device identities, battery
models, price/weather state, recent history, and long-format time-series
samples. Samples older than the recent retention window are rolled off to
Parquet under `cold_dir`.

`cold_retention_days` bounds the cold Parquet tier: day files older than
this are deleted by the hourly rolloff. The default `0` keeps everything —
a year of ~50 metrics is a few GB, so bounding is opt-in for small SD
cards. The rolloff loop also warns (log + event feed, at most daily) when
free disk drops below 500 MB.

Changing `state.path` or `state.cold_dir` requires restart.

## `price` and `weather`

```yaml
price:
  provider: elprisetjustnu
  zone: SE3
  grid_tariff_ore_kwh: 50
  vat_percent: 25

weather:
  provider: met_no
  latitude: 59.3293
  longitude: 18.0686
  pv_rated_w: 10000
```

These feed the planner and ML twins. Provider changes are picked up by the
next fetch cycle.

## `v2x` — bidirectional EV policy (optional)

```yaml
v2x:
  enabled: false                  # default-off; planner does not command V2X yet
  driver_name: ferroamp_dc2       # optional; empty applies to every V2X driver
  vehicle_capacity_wh: 77000      # optional if the charger reports capacity
  min_reserve_soc_pct: 35         # required >0 when enabled
  departure_target_soc_pct: 80    # optional; pair with departure_time
  departure_time: "07:30"         # HH:MM local next occurrence, or RFC3339
  max_charge_w: 7000
  max_discharge_w: 5000
  export_allowed: false
  grid_charging_allowed: false
  cycle_cost_ore_kwh: 12
```

This policy is read-only input for `GET /api/v2x/policy` and the
`v2x_policy` block in `/api/status`. It answers what V2X power range is safe
right now from live connection state, vehicle SoC, reserve/departure rules,
charger limits, and grid import/export state. It does not enable automatic
planner dispatch; manual V2X commands still go through `POST /api/v2x/command`.

## `planner`

```yaml
planner:
  enabled: true
  engine: python
  mode: self_consumption
  horizon_hours: 48
  interval_min: 15
  soc_min_pct: 10
  soc_max_pct: 90
  optimizer_solver: HIGHS
  optimizer_formulation: auto
  optimizer_transport: auto
  optimizer_socket: /run/ftw-optimizer/optimizer.sock
  optimizer_timeout_s: 30
  optimizer_idle_timeout_s: 120
  optimizer_mip_rel_gap: 0.005
  optimizer_cvar_weight: 0.15
  optimizer_cvar_alpha: 0.90
  optimizer_recourse_shadow: false
  optimizer_recourse_non_anticipative_slots: 1
  optimizer_challenger_policy: multistage
  optimizer_multistage:
    scenario_limit: 12
    branch_interval_slots: 4
    branch_horizon_slots: 48
    max_branching: 2
    near_horizon_slots: 16
    mid_horizon_slots: 96
    mid_block_slots: 2
    far_block_slots: 4
    service_cvar_weight: 1.0
    service_cvar_alpha: 0.95
    economic_cvar_weight: 0
    economic_cvar_alpha: 0.90
    decomposition_threshold: 20
    decomposition_method: auto
    ph_max_iterations: 8
    ph_rho: 50
    ph_tolerance_w: 5
```

The planner emits grid-target slots that the control loop consumes in
`planner_*` modes. Planner service wiring happens at startup, so some
structural changes still need restart.

The Python/CVXPY engine is primary. `engine: dp` selects the former in-process
planner for emergency rollback. Any worker timeout, solver failure, or rejected
trajectory automatically uses that DP for the affected replan. Full model,
deployment, validation, and replay details are in [optimizer.md](optimizer.md).

`optimizer_transport` is `process` by default for native/backwards-compatible
installs. Official Compose sets it to `auto`: use the Unix sidecar when its
protocol-v1 handshake succeeds, otherwise retry through the bundled process.
`unix` makes the socket mandatory at the Python boundary (Go DP remains the
planner fallback).

## `device_repository`

```yaml
device_repository:
  enabled: true
  refresh_interval_h: 24
  repositories:
    - id: ftw-official
      name: FTW official drivers
      manifest_url: https://github.com/srcfl/ftw/releases/download/drivers-stable/manifest.json
      enabled: true
      trusted_keys:
        ftw-drivers-2026-01: MX+j27UBkyM099hTyJlmMLK9qlTTDUJsaK/vH12fFKc=
```

Every enabled repository must pin at least one Ed25519 public key or explicitly
set `allow_unsigned: true`. The latter and `allow_insecure` exist for local
development only. Refresh updates the last-good cache; installation and driver
restart are a separate operator action.

Drivers are published from this monorepo but on their own edge/beta/stable
release channels. See [Independent driver repository](device-repository.md).
Because the official public signing key is pinned in FTW, the shorter explicit
opt-in below resolves to the same stable repository:

```yaml
device_repository:
  enabled: true
```

Omitting `device_repository` entirely remains bundled-only and performs no
repository network requests.

`optimizer_recourse_shadow` runs a storage-only stochastic challenger after the
active champion solve. `optimizer_challenger_policy` selects the two-stage
`recourse` reference or the hierarchical `multistage` policy. The first slot is
non-anticipative by default; later storage decisions may adapt only after their
scenario-tree observation. The challenger is scored
against the champion with independent virtual battery state and can never feed
dispatch. Shadow evaluation pauses while a flexible EV or thermal contract is
active, because those assets do not yet have equivalent stateful telemetry in
the evaluator.

The multistage defaults retain at most 12 net-energy/PV trajectory medoids, branch every
four slots through the first 12 hours, keep the first four hours at 15-minute
resolution, and move-block the far horizon to hourly decisions. Service CVaR is
solved lexicographically before expected economic cost. `auto` decomposition
uses Progressive Hedging only for eligible continuous arbitrage ensembles above
the threshold; discrete cases are reduced and solved as an exact extensive
HiGHS model. Under ordinary tariffs `auto` is an LP. It adds binary cycling or
meter guards only when negative import prices or inverted import/export prices
make simultaneous flows economically unsafe.

## `batteries`

Optional per-battery overrides keyed by configured driver name:

```yaml
batteries:
  ferroamp:
    soc_min: 0.10
    soc_max: 0.95
    max_charge_w: 8000
    max_discharge_w: 8000
    weight: 2.0
```

Prefer per-driver `max_charge_w` / `max_discharge_w` for hardware command
caps. The `batteries` map remains useful for strategy weights and explicit
SoC policy.

## EV, OCPP, Notifications, Nova

Additional optional blocks exist for:

- `ocpp`: embedded OCPP 1.6J Central System.
- `ev_charger`: Settings-UI managed cloud/local EV charger config.
- `loadpoints`: planner-visible EV loadpoint contracts.
- `notifications`: ntfy push notifications.
- `nova`: Sourceful Nova federation publishing.

Use `config.example.yaml`, the Settings UI, and the corresponding docs for
these specialized blocks.

## Hot Reload

| Field area | Hot? | Notes |
|---|---|---|
| `site.grid_target_w`, `site.grid_tolerance_w` | yes | Applied on the next control cycle. |
| `site.slew_rate_w`, `site.slew_enabled`, `site.min_dispatch_interval_s` | yes | Applied to dispatch immediately after reload. |
| `site.control_interval_s` | partly | The next sleep interval picks it up; current tick keeps running. |
| `site.watchdog_timeout_s` | yes | Used by watchdog scan. |
| `site.troubleshooting_mode` | yes | Restarts active drivers to pass `_troubleshooting_mode`; no control behavior change. |
| `fuse.*` | yes | Read by the control loop. |
| `drivers[]` add/remove/reconfigure | yes | Registry diffs and respawns affected drivers. |
| `drivers[].lua` path change | yes | Driver restarts with the new file. |
| Editing a Lua file in place | no | Restart or touch `config.yaml` to reload the driver. |
| `v2x.*` | yes | API readback uses the current config; dispatch does not consume it yet. |
| `api.port` | no | Socket bind happens at startup. |
| `state.path`, `state.cold_dir` | no | Store opens at startup. |
| `homeassistant.*` | no | Broker connection is built at startup. |
| `caldav.*` | no | The in-process server and calendar service are built at startup. |
| `price.*`, `weather.*` | yes | Picked up by the next fetch. |

When in doubt, restart the service after structural changes.

## API Examples

```bash
curl http://localhost:8080/api/config

curl -X POST http://localhost:8080/api/config \
  -H 'Content-Type: application/json' \
  -d @new-config.json

curl -X POST http://localhost:8080/api/mode \
  -H 'Content-Type: application/json' \
  -d '{"mode":"self_consumption"}'

curl -X POST http://localhost:8080/api/target \
  -H 'Content-Type: application/json' \
  -d '{"grid_target_w":0}'
```
