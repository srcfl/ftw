# Configuration

forty-two-watts is configured by **one yaml file** (`config.yaml`). The Settings UI in the web dashboard reads and writes the same file via the REST API. Everything is hot-reloadable except a small set of one-time bindings (HA broker connection, API socket port, redb file path).

## Two equivalent paths

```
                  ┌────────────────────────────┐
                  │       config.yaml          │
                  └────────────┬───────────────┘
                               │
              ┌────────────────┴────────────────┐
              ▼                                 ▼
    ┌──────────────────┐              ┌─────────────────┐
    │ Edit yaml in     │              │ Open Settings   │
    │ your editor      │              │ UI in browser   │
    └────────┬─────────┘              └────────┬────────┘
             │ save                            │ click Save
             ▼                                 ▼
    ┌──────────────────┐              ┌─────────────────┐
    │ notify watcher   │              │ POST            │
    │ debounces 500ms  │              │ /api/config     │
    └────────┬─────────┘              └────────┬────────┘
             │                                 │
             └────────┬────────────────────────┘
                      ▼
            ┌────────────────────┐
            │ Diff vs current,   │
            │ apply per-system,  │
            │ swap RwLock<Config>│
            └────────────────────┘
```

Whichever path you use, the runtime ends up at the same place.

## Schema

### `site` — control loop

```yaml
site:
  name: "Heart of Gold"           # cosmetic, shown in UI
  control_interval_s: 5           # how often the control loop runs
  grid_target_w: 0                # PI setpoint. 0 = self-consumption
  grid_tolerance_w: 42            # deadband — no dispatch within ±42W
  watchdog_timeout_s: 60          # revert driver to autonomous if it stalls
  smoothing_alpha: 0.3            # legacy EMA alpha (Kalman is now used internally)
  gain: 0.5                       # legacy proportional gain
  slew_rate_w: 500                # max change in dispatch target per cycle
  min_dispatch_interval_s: 5      # holdoff between successive dispatches
  max_export_w: 0                 # cap total site export (W); 0 = fuse only
```

`max_export_w` is an **opt-in export ceiling below the physical fuse**.
Some inverters trip into a protective fault on *sustained* grid export
well under the breaker rating — a Ferroamp EnergyHub, for example, faults
(state `0x8030`) after ~8 kW of continuous midday export and only recovers
as PV wanes, losing hours of solar. Set `max_export_w` just below the
observed trip point and the EMS enforces it two ways: the dispatch fuse
guard scales battery discharge back so predicted export stays under it,
and the MPC caps each plan slot's export so the planner never *schedules*
a discharge that would over-export. `0` (default) leaves export bounded
only by the fuse. Hot-reloads on the dispatch side; the MPC picks it up on
restart (parity with `fuse.max_amps`). See [safety.md §3c](safety.md).

### `fuse` — shared breaker limit

```yaml
fuse:
  max_amps: 16                    # main fuse rating
  phases: 3                       # 1 or 3
  voltage: 230                    # nominal phase voltage
```

`max_power_w = max_amps × voltage × phases`. The fuse guard scales discharge targets when `total_pv + total_discharge > max_power_w`.

### `drivers` — devices

```yaml
drivers:
  - name: ferroamp                # unique key, used in API + HA topics
    lua: drivers/ferroamp.lua     # path to driver script
    is_site_meter: true           # exactly one driver must have this
    battery_capacity_wh: 15200    # 0 (or unset) → driver's battery emits are dropped
    max_charge_w: 10000           # per-driver cap; 0/unset → 5 kW default
    max_discharge_w: 10000
    inverter_group: ferroamp      # optional — see "Inverter affinity" below
    mqtt:
      host: 192.168.1.153
      port: 1883
      username: extapi
      password: ferroampExtApi

  - name: sungrow
    lua: drivers/sungrow.lua
    battery_capacity_wh: 9600
    inverter_group: sungrow
    modbus:
      host: 192.168.1.10
      port: 502
      unit_id: 1
```

Each driver must have **either** `mqtt` or `modbus` (not both, not neither). Adding/removing/changing a driver hot-reloads — the matching thread spawns or stops within a few seconds.

#### Per-driver power limits (`max_charge_w`, `max_discharge_w`)

Both are optional. When unset (or set to `0`), the dispatcher uses the
**5 kW default** (`MaxCommandW`) — the conservative floor the EMS
shipped with during early v0.x. Lift these per driver once you know
the real hardware capability:

- Ferroamp EnergyHub commonly delivers 10–15 kW continuous charge.
- Sungrow SH10RT-V13 is rated ~10 kW hybrid.
- Pixii EssLi is ~5–7 kW.

The two directions are independent — hybrid inverters frequently have
asymmetric charge/discharge rating, so set both explicitly for any
driver you tune.

Regardless of how high you set the per-driver caps, the site
fuse-guard (derived from `fuse.max_amps × voltage × phases`) stays the
non-negotiable ceiling: it protects both the import (charge-heavy) and
export (discharge+PV-heavy) sides of the grid boundary, scaling
targets down in the direction that would otherwise blow the fuse.

Planned follow-up (#145 Phase B/C): observed rolling maxima + self-tune
probing so the UI can suggest the right value from measurement rather
than operator guesswork.

#### Inverter affinity (`inverter_group`)

Tag drivers that share a single physical inverter unit with the same
`inverter_group` string. The dispatcher uses this to keep charging flows
DC-coupled: PV surplus on inverter A is routed to battery A via A's own
DC path, avoiding the DC→AC→AC→DC round-trip that cross-charging
(PV on A → AC bus → B → battery B) would incur. Typical site layout:

- A hybrid inverter like Ferroamp with its own PV strings *and* a battery
  gets `inverter_group: ferroamp` on its driver entry.
- A Sungrow hybrid gets `inverter_group: sungrow`.
- A separate PV-only driver reading a string on the Ferroamp side should
  share its tag: `inverter_group: ferroamp`.
- A standalone AC-coupled battery (no local PV) can either leave the tag
  unset or use a unique group — both keep that battery in the "overflow"
  pool that accepts cross-routed energy when PV-local capacity is
  exhausted.

When no drivers carry the tag, or only one group exists, the dispatcher
falls back to its capacity-proportional split (the default through
v0.27). See issue #143 for the design rationale. Estimated benefit on a
two-inverter site with balanced PV strings: ~3-4 % round-trip efficiency
improvement during the hours when the plan chooses to charge from PV.

#### Ferroamp per-driver SoC bounds (`config.charge_ceil_soc`, `config.discharge_floor_soc`)

The Ferroamp Lua driver gates per-ESO charge/discharge dispatch on two
SoC bounds. Both are read from the driver's `config:` block and default
to the existing hardcoded values when unset.

```yaml
  - name: ferroamp
    lua: drivers/ferroamp.lua
    is_site_meter: true
    config:
      # Optional. Defaults shown — both fields are read by the Lua
      # driver to gate per-ESO charge/discharge dispatch. Ferroamp's
      # own BMS still protects against overcharge / deep discharge,
      # so these are tuning knobs for cell-balancing / longevity
      # preferences, not safety limits.
      charge_ceil_soc: 0.95      # exclude ESOs at or above this SoC from charge dispatch
      discharge_floor_soc: 0.15  # exclude ESOs at or below this SoC from discharge dispatch
```

Validation: `charge_ceil_soc` must be in `(0, 1.0]`; `discharge_floor_soc`
must be in `[0, 1.0)`. Out-of-range or non-numeric values log a warning
and keep the default. Note that the planner's `soc_max_pct` is an
independent layer — to actually charge to 100 % both caps must be lifted.

#### Ferroamp PV-curtail release watts (`config.pplim_release_w`)

Required if you set `supports_pv_curtail: true` on the Ferroamp driver
and want the dispatcher's automatic curtail-release to take effect.

**Background.** Ferroamp's extapi treats `{"cmd":{"name":"pplim","arg":0}}`
as "limit PV output to 0 W" — the same wire bytes a naive release
would have, opposite semantics. The inverter then sticks at 0 W PV
until the operator clears pplim from the Ferroamp portal or
power-cycles the EnergyHub. The driver therefore refuses to publish
`pplim arg=0` from any path, and the default `curtail_disable`
handler is a no-op.

To recover automatic release, set `pplim_release_w` to the inverter's
nominal max (e.g. 15000 for a 15 kW SSO). On `curtail_disable` the
driver publishes `pplim arg=<pplim_release_w>` which Ferroamp accepts
as "raise the limit so PV can run free".

```yaml
  - name: ferroamp
    lua: drivers/ferroamp.lua
    supports_pv_curtail: true
    config:
      pplim_release_w: 15000  # SSO nominal max — adapt to your inverter
```

If left unset (default `0`), curtail still works (pplim arg=N for
N > 0), but the release path is a no-op and the operator must clear
pplim manually from the Ferroamp portal once curtailment ends.

The 2026-05-27 incident: dispatching `pplim arg=0` during a stale
curtail-allocation cycle locked the SSO at 0 W PV for 30+ minutes
and triggered a fault state on the EnergyHub.

#### Ferroamp stuck-pplim self-healing watchdog

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

### `api` — REST + web UI

```yaml
api:
  port: 8080                      # not hot-reloadable (socket bind happens at startup)
```

### `homeassistant` — MQTT bridge (optional)

```yaml
homeassistant:
  enabled: true
  broker: 192.168.1.1
  port: 1883
  username: homeems
  password: homeems
  publish_interval_s: 5
```

Not hot-reloadable: changing requires a restart for the broker connection to be re-established.

### `state` — redb persistence (optional)

```yaml
state:
  path: state.redb                # default; not hot-reloadable
```

### `price` — spot price source (optional)

```yaml
price:
  provider: elprisetjustnu        # or "entsoe" or "none"
  zone: SE3                       # SE1, SE2, SE3, SE4
  grid_tariff_ore_kwh: 50         # added on top of spot
  vat_percent: 25                 # default 25% (Sweden)
  api_key: null                   # required for entsoe
```

`elprisetjustnu` is free and key-less but Sweden-only. ENTSO-E covers all of EU but needs a free API key.

### `weather` — forecast source (optional)

```yaml
weather:
  provider: met_no                # or "openweather" or "none"
  latitude: 59.3293
  longitude: 18.0686
  api_key: null                   # required for openweather
```

`met.no` is free and key-less. Default coordinates point at Stockholm.

### `batteries` — per-battery overrides (optional)

```yaml
batteries:
  ferroamp:
    soc_min: 0.10                 # don't discharge below 10% (overrides BMS hint)
    soc_max: 0.95                 # don't charge above 95%
    max_charge_w: 5000
    max_discharge_w: 5000
    weight: 2.0                   # used by Mode::Weighted
  sungrow:
    weight: 1.0
```

Keys must match `drivers[].name`. Leave blank to use BMS defaults.

## Hot-reload matrix

| Field | Hot? | Notes |
|-------|------|-------|
| `site.grid_target_w` | ✅ | Updates PID setpoint live |
| `site.grid_tolerance_w` | ✅ | Deadband applied next cycle |
| `site.slew_rate_w` | ✅ | Applied next dispatch |
| `site.min_dispatch_interval_s` | ✅ | |
| `site.max_export_w` | ⚠️ | Dispatch guard hot-reloads; MPC picks it up on restart |
| `site.control_interval_s` | ⚠️ | Picked up next cycle (current cycle uses old value) |
| `site.watchdog_timeout_s` | ✅ | |
| `fuse.*` | ✅ | Read fresh each cycle |
| `drivers[]` add/remove | ✅ | `DriverRegistry::reload()` diffs and applies |
| `drivers[].lua` change | ✅ | Driver thread restarts |
| `drivers[].mqtt/modbus` change | ✅ | Driver thread restarts |
| `drivers[].battery_capacity_wh` | ✅ | Driver thread restarts (capacity affects dispatch math) |
| `api.port` | ❌ | Restart required |
| `homeassistant.*` | ❌ | Restart required (broker reconnect not implemented) |
| `state.path` | ❌ | redb file opened at startup |
| `price.*` | ✅ | Picked up next price-fetch cycle |
| `weather.*` | ✅ | Picked up next weather-fetch cycle |
| `batteries.*` | ✅ | Read fresh each control cycle |

## Atomic writes

`save_atomic()` writes to `config.yaml.tmp` then renames over `config.yaml`. The rename is atomic on POSIX, so partial writes never appear to the file watcher. If your editor uses backup-and-replace (vim, helix, vscode), the watcher may fire twice; the 500ms debounce coalesces them.

## API

```bash
# Get current full config
curl http://localhost:8080/api/config

# Replace + hot-apply + save yaml
curl -X POST http://localhost:8080/api/config \
  -H 'Content-Type: application/json' \
  -d @new-config.json

# Quick mode/target setters (don't touch yaml)
curl -X POST http://localhost:8080/api/mode -d '{"mode":"self_consumption"}'
curl -X POST http://localhost:8080/api/target -d '{"grid_target_w": 0}'
```
