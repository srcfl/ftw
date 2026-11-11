# ADR 0012: Heat control from a target temperature and a learned thermal twin

- Status: proposed. The owner has answered the open questions (see
  [Decisions](#decisions)); the ADR is accepted when this PR merges. The
  declaration ([#1995](https://github.com/srcfl/ftw/pull/1995)) and the twin
  in shadow ([#1996](https://github.com/srcfl/ftw/pull/1996)) are proposed as
  PRs. Planned control has not shipped.
- Date: 2026-11-10
- Decided by: Fredrik on 10 November 2026, in his
  [answers on #1994](https://github.com/srcfl/ftw/pull/1994#issuecomment-6763083595).
  See [Decisions](#decisions).
- Builds on: [#520](https://github.com/srcfl/ftw/issues/520), the
  [`thermal`](../../go/internal/thermal/model.go) contract, driver
  [`controls`](../../go/internal/drivers/catalog_controls.go), the
  [operator control API](../../go/internal/api/api_driver_control.go) and the
  learned heating coefficient in [`loadmodel`](../../go/internal/loadmodel/model.go).
- Roadmap: Fredrik moves heat, space heat included, ahead of
  [Heat: data first, bounded control later](../roadmap.md) and updates that
  section and VISION.md himself.
- Layering: [#1772](https://github.com/srcfl/ftw/pull/1772). The twin and
  the planner are a layer that proposes values; Core checks them.

## Context

Read against `master` at `7350a3e`:

- A driver can declare `controls` in its `DRIVER` block: an id, a label, an
  evidence kind and one typed input with bounds. Heishamon declares
  `set_heat_curve_offset` (−3..+3 °C, `write_ack`).
- `POST /api/drivers/{name}/control` sends a declared control, clamps it to
  the declared bounds and holds it for at most 24 h. `DELETE` and expiry call
  the driver's own `driver_default_mode`. The Heating view renders it.
- `thermal.DecideIntent` is a threshold rule over a temperature band and a
  price. No package imports `thermal`.
- `loadmodel` learns a whole-house heating coefficient (W/°C) from load and
  outdoor temperature. It has no indoor temperature, no inertia and no
  control input.
- The control tick sends `battery`, `curtail` and the opt-in `solar_pv`
  hint. Nothing sends a heat command.
- The Energyplan request already carries `thermal_loads` and the result
  `thermal_power_w` and `thermal_state`, but protocol v1 marks thermal
  inputs unsupported and Core rejects a non-empty thermal result
  ([contract](../energyplan-contract.md)).

A fixed threshold rule cannot answer the household's real question: *how
long can this house coast, and how early must it start?* That depends on the
building's inertia, the outdoor forecast and how strongly and how late the
device responds. These differ by orders of magnitude between a concrete
floor heated by a ground-source pump, a timber house with an air-to-air
unit, and a hot-water tank. An owner should not have to know or enter them.

## Decision

**The owner states a target temperature. A layer learns each zone's thermal
twin from what Core records: heat loss, inertia, and how the device's control
moves the temperature. A planner in the same layer uses the twin, prices,
solar and weather forecasts to propose the control value for each slot
(on/off, setpoint or offset) that keeps the predicted temperature on target
at the lowest cost, so heat joins battery and car in one plan of
flexibility. Core checks every proposed value against what Core owns: the
declared bounds, the lease, the minimum on and off times, and release to the
device when a reading is stale or missing.** Core never learns a device by
name, and Core is not what limits which devices work: support is claimed per
driver, with hardware evidence.

### 1. What the owner sets

A zone is one comfort sensor with its own target. A house has as many zones
as it has sensors the owner cares about. Each is off by default. The driver
names below are examples of what a site might run; Core names none:

```yaml
thermal:
  zones:
    - id: living
      enabled: true
      comfort_sensor: { driver: wiser, metric: room_living_temp_c }
      target_c: 21.0
      flex_c: 1.0        # how far the planner may move away from target
      controls:
        - { driver: heishamon, id: set_heat_curve_offset }   # shared source
        - { driver: wiser, id: set_room_living_setpoint }    # this room only
    - id: bedroom
      enabled: true
      comfort_sensor: { driver: wiser, metric: room_bedroom_temp_c }
      target_c: 18.0
      flex_c: 1.5
      controls:
        - { driver: heishamon, id: set_heat_curve_offset }
        - { driver: wiser, id: set_room_bedroom_setpoint }
    - id: hot_water
      enabled: true
      comfort_sensor: { driver: heishamon, metric: hp_dhw_temp_c }
      target_c: 60.0
      flex_c: 5.0        # the band 55-65 °C; a dhw zone never goes under 55
      controls:
        - { driver: heishamon, id: set_dhw_mode }
```

- `target_c` is the goal for that zone alone. An optional schedule may
  change it over the day later; this ADR does not add one.
- A zone lists every control that heats it. A control listed by several
  zones is a **shared source**, such as one heat pump's curve offset serving
  every room. A control listed by one zone is **local**, such as a room
  thermostat or a valve.
- `flex_c` is the owner's trade between comfort and savings. Around the
  target it gives the hard band `[target − flex, target + flex]`, which the
  learned twin may never widen.
- **A hot-water zone has a floor.** A zone whose comfort sensor is declared
  `asset = "dhw"` never gets a band below 55 °C, whatever its `target_c`
  and `flex_c`. Stored water that sits around 45 °C lets Legionella grow, and
  Swedish building rules (BBR 6:62) ask for at least 50 °C at the tap. Core
  owns the floor: it rejects a configuration whose band goes under it, and
  releases the tank to the device when the measured temperature is under it.
  The value 55 °C is proposed, not decided; see [Open](#open).
- Without a comfort sensor, a zone cannot be planned. The roadmap does not
  let Core assume indoor temperature.

### 2. Declaration by the driver

Two optional fields on an existing control, an optional `sensors` list and
optional device protection timings. A control without `asset` and
`strategy` stays operator-only.

```lua
controls = {
  {
    id = "set_heat_curve_offset", label = "Heat curve offset",
    evidence = "write_ack",
    asset = "space_heat",          -- space_heat | dhw | cooling | buffer
    strategy = "curve_offset",     -- curve_offset | setpoint | on_off | discrete_mode
    zone = "z1",
    min_interval_s = 600,          -- vendor rate limit or wear
    device_hold_max_s = 3600,      -- the device reverts by itself after this
    input = { type = "number", min = -3, max = 3, step = 1, unit = "°C", default = 0 },
  },
  {
    id = "set_power", label = "Heating on",
    asset = "space_heat", strategy = "on_off",
    min_on_s = 1200, min_off_s = 900,   -- compressor protection
    input = { type = "boolean" },
  },
},
sensors = {
  { metric = "hp_indoor_temp_c", asset = "space_heat", zone = "z1" },
  { metric = "hp_dhw_temp_c",    asset = "dhw" },
},
```

- `on_off` covers devices whose only lever is start/stop. The driver's
  `min_on_s` and `min_off_s` are hard constraints on the plan, not
  preferences.
- **The lease.** `device_hold_max_s` says the device itself keeps a value
  for at most that long and then returns to its own regulation, such as a
  hub's timed boost or override. Core sends each planned value with a hold
  no longer than that and renews it while the plan stands. If FTW stops,
  crashes or loses power, the device falls back on its own. A control
  without a device-side hold may still be planned, but only within one
  declared `step` of its declared `input.default`. Without both a default
  and a step, it gets no planned control.
- Supporting a device is a driver's work: a user can write a driver for
  their own device, declare these fields and test it. Support is claimed
  per driver with hardware evidence, not by Core.
- `sensors` names metrics a driver already emits. Any driver can supply
  them, so a room thermostat's sensor can judge a heat pump driven by
  another driver.
- The parser ignores unknown fields, so existing drivers are unaffected.

### 3. The thermal twin

One first-order model per zone, learned in a layer beside `loadmodel` and
the per-battery response model. It reads what Core records and never holds
up the control loop:

```text
C · dT_in/dt = G(u, T_out) − UA · (T_in − T_out)
```

| Learned term | Meaning | Learned from |
|---|---|---|
| `UA` | heat loss, W/°C | indoor, outdoor and heat input; seeded from `loadmodel` |
| `τ = C/UA` | inertia: how long the zone coasts | indoor temperature drift while input is steady |
| `G(u, T_out)` | how much heat the control value delivers | indoor response to past control changes |
| dead time | how late the response starts | the same, as a delay |

For a hot-water tank or buffer, the same form describes the store's
temperature, standing loss and fill rate.

- **Shadow first.** A new twin only predicts. It is compared with the
  measured temperature and is not used for control.
- **Confidence gate.** A zone gets planned control only while its twin is
  trusted: its prediction error has stayed under a limit across a stated
  period. An untrusted twin gives no planned control, and the device runs
  itself. The twin's state ("learning", "trusted", "drifting") and its error
  are shown to the owner.
- **Learning from ordinary operation.** The twin learns from what the device
  does by itself and from owner holds. The planner makes no steps only to
  learn.
- The twin is advisory, as [safety.md](../safety.md#planner-and-model-containment)
  requires. It never widens the band or a declared limit, and its prediction
  never counts as evidence. Only the measured temperature verifies a plan.

### 4. The planner

- **A planning layer proposes; Core checks.** A dynamic program over the
  existing price, PV and weather horizon. Its state is the predicted zone
  temperature, and the actions are the declared control values, few by
  construction (for example seven offsets, or on/off). The cost is energy
  at its marginal price plus a penalty for distance from the target. The
  band and the `min_on_s`/`min_off_s` timings are constraints of the plan.
  Core checks them again on every value it is asked to send.
- **Zones are planned together.** Every enabled zone must stay inside its
  own band. A shared source is planned once for all the zones it serves, so
  the zone that needs the most heat sets it. Local controls then trim each
  room toward its own target, and the twin of each zone learns its response
  to both. A shared source is never raised beyond what keeps the warmest
  zone inside its band, unless that zone's local control can hold it back.
  When no value satisfies every zone, the plan is infeasible. The shared
  source is then released to the device and the conflict is shown to the
  owner, not hidden by favouring one room. There is no zone ranking.
- **The planner works out the buffering itself.** It preheats before an
  expensive period exactly as far as the learned inertia lets the zone
  coast, and stores PV surplus as heat where the twin says it will last.
- **Energyplan can take over later** through the reserved `thermal_loads`
  and `thermal_state` fields, behind an explicit worker capability, as
  `hourly_import_caps` did. Core then validates its trajectory against the
  same bounds and keeps the Go planner as fallback.
- `thermal.DecideIntent` and the other unused `thermal` code are removed.
  Without a trusted twin there is no rule-based fallback: the device runs
  itself.

### 5. Dispatch and ownership

- A heat block in the control tick, after the solar feed, on
  freshness-allowed ticks only. A stale site meter already defaults every
  driver.
- Write when the planned value changes, and renew the device-side hold
  before it ends. Never faster than `min_interval_s`.
- One owner per driver. Operator holds and planned commands share the
  per-driver state now in `api`, moved to a package both can use. An
  operator hold wins. The planner treats the held control as fixed at its
  held value for every zone it serves, and plans the rest around it until
  the hold ends.
- Each slot records the planned value, the command, the driver result, the
  predicted temperature and the measured temperature. That is the evidence
  the product rules ask for, and it is what the twin learns from.

### 6. Safety

Each protection follows [safety.md](../safety.md#clamp-discipline). Core
owns the first group. The layer's checks only decide what it proposes, and
Core does not rely on them.

| Owner | Condition | Limit | Reaction and recovery |
|---|---|---|---|
| Core | Value outside the declared input | declared min/max | clamp (exists today) |
| Core | No device-side hold | one `step` from `input.default` | refuse a value beyond it |
| Core | FTW stops renewing | `device_hold_max_s` | the device reverts by itself |
| Core | Comfort reading stale or missing | the watchdog timeout | release to device; resume on a fresh reading |
| Core | Short cycling | `min_on_s`, `min_off_s` | refuse a value that breaks it |
| Core | Writes too frequent | `min_interval_s` | skip; the next due slot sends |
| Core | Hot-water zone under its floor | 55 °C, see [Open](#open) | reject the band in config; release the tank to the device; resume above the floor |
| Layer | Measured temperature outside the band | owner band | propose release; resume back inside |
| Layer | Twin not trusted, or error over limit | confidence gate | propose nothing; the device runs itself |
| Layer | Zones conflict on a shared source | every zone's band | release the source and tell the owner |

The site-meter and watchdog paths stay ahead of all of these.

When the layer is down, nothing reacts to a measured temperature outside the
band. That case rests on the lease alone: a device with a device-side hold
returns to its own regulation within `device_hold_max_s`, and a control
without one can never be more than one `step` from its `input.default`. The
hot-water floor does not depend on the layer, because Core owns it.

### Not in scope

Temperature schedules, the Live Energy node for heat pumps (#520, part 1),
the Settings form and an Energyplan thermal capability. Each is a separate
change.

## Decisions

Fredrik answered on 10 November 2026:

1. **Order.** Go ahead, starting with the twin in shadow. Heat pumps, space
   heat included, belong in FTW as long as the base model is good. Planning
   flexibility across battery, car and heat is the goal.
2. **Lease.** Planned control needs the device to fall back on its own,
   through a device-side timed hold. Otherwise planned values stay a small
   step from the device default.
3. **Untrusted twin.** No planned control; the device runs itself.
   `thermal.DecideIntent` and the other unused `thermal` code are removed.
4. **Learning steps.** Not for now. The twin learns from ordinary operation
   and owner holds.
5. **Zone conflicts.** Release the shared source and tell the owner. No zone
   ranking.
6. **First case.** None named in Core. Build the generic base so that a user
   can write and test a driver for their own device. Support is claimed per
   driver, with hardware evidence.
7. **Keep the core simple.** Core owns the declared bounds, the lease, the
   minimum on and off times, and release to the device when a reading is
   stale or missing. The twin and the planner are a layer that proposes
   values, and Core checks them.

## Open

- **The hot-water floor.** Proposed at 55 °C in review, for Fredrik to
  confirm or change. A periodic pass to 60 °C or more against Legionella is
  a later, separate change.
