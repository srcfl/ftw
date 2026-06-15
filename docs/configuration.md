# Configuration

forty-two-watts is configured by one YAML file, normally `config.yaml`.
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
```

`grid_target_w` is the PI setpoint in site convention. `0` means
self-consumption around the site meter. `slew_rate_w` is a soft per-cycle
ramp ceiling; per-driver power caps and the fuse guard remain the hard
dispatch constraints.

### Global Troubleshooting Mode

Use **Settings → Control → Troubleshooting mode** when diagnosing a live
system issue. It does not change planner, dispatch, clamp, or driver command
behavior. It adds dispatch-decision log lines, exposes the active flag in
`/api/status`, and passes a reserved `_troubleshooting_mode` flag to Lua
drivers so driver-specific diagnostics can emit richer status/readback data.

Turn it off in Settings after the incident; logs and long-format metrics are
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

### Pixii Diagnostics

When Troubleshooting mode is enabled from Settings, the Pixii driver emits
extra SunSpec battery diagnostics: charge status, control mode, battery state,
vendor state, event bits, and setpoint readback. `charge_status=testing` is
the useful signal for suspected Pixii calibration/testing sessions that may
ignore external setpoints.

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

## `state`

Persistent state uses SQLite:

```yaml
state:
  path: state.db
  cold_dir: cold
```

The SQLite DB stores config overrides, events, device identities, battery
models, price/weather state, recent history, and long-format time-series
samples. Samples older than the recent retention window are rolled off to
Parquet under `cold_dir`.

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

## `planner`

```yaml
planner:
  enabled: true
  mode: self_consumption
  horizon_hours: 48
  interval_min: 15
  soc_min_pct: 10
  soc_max_pct: 90
```

The planner emits grid-target slots that the control loop consumes in
`planner_*` modes. Planner service wiring happens at startup, so some
structural changes still need restart.

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

## EV, V2X, OCPP, Notifications, Nova

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
| `fuse.*` | yes | Read by the control loop. |
| `drivers[]` add/remove/reconfigure | yes | Registry diffs and respawns affected drivers. |
| `drivers[].lua` path change | yes | Driver restarts with the new file. |
| Editing a Lua file in place | no | Restart or touch `config.yaml` to reload the driver. |
| `api.port` | no | Socket bind happens at startup. |
| `state.path`, `state.cold_dir` | no | Store opens at startup. |
| `homeassistant.*` | no | Broker connection is built at startup. |
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
