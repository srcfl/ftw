# ADR 0012: Heat control from a target temperature and a learned thermal twin

- Status: proposed. Nothing described here has shipped.
- Date: 2026-11-10
- Decided by: nobody yet. The open questions at the end are for Fredrik.
- Builds on: [#520](https://github.com/srcfl/ftw/issues/520), the
  [`thermal`](../../go/internal/thermal/model.go) contract, driver
  [`controls`](../../go/internal/drivers/catalog_controls.go), the
  [operator control API](../../go/internal/api/api_driver_control.go) and the
  learned heating coefficient in [`loadmodel`](../../go/internal/loadmodel/model.go).
- Roadmap: [Heat: data first, bounded control later](../roadmap.md). This
  proposes the shape of that later step; it does not move it earlier.

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

**The owner states a target temperature. FTW learns each zone's thermal
twin from what it measures: heat loss, inertia, and how the device's control
moves the temperature. A planner uses the twin, prices, solar and weather
forecasts to choose the control value for each slot (on/off, setpoint or
offset) that keeps the predicted temperature on target at the lowest cost.
Core validates every value against the declared bounds and the owner's
limits. It gives the device back to its own regulation whenever the twin is
not trusted, the measurement leaves the comfort range, or the lease runs
out.** Core never learns a device by name.

### 1. What the owner sets

A zone is one comfort sensor with its own target. A house has as many zones
as it has sensors the owner cares about. Each is off by default:

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
      target_c: 50.0
      flex_c: 5.0
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
    input = { type = "number", min = -3, max = 3, step = 1, unit = "°C" },
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
- `sensors` names metrics a driver already emits. Any driver can supply
  them, so a room thermostat's sensor can judge a heat pump driven by
  another driver.
- The parser ignores unknown fields, so existing drivers are unaffected.

### 3. The thermal twin

One first-order model per zone, learned in Core beside `loadmodel` and the
per-battery response model:

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
- **Confidence gate.** Planned control starts only when the twin's
  prediction error has stayed under a limit across a stated period that
  includes control changes. The Heating view shows the twin's state
  ("learning", "trusted", "drifting") and its error.
- **Learning needs variation.** Ordinary operation and owner holds provide
  some. While a zone is still in shadow, the planner may use small steps
  that stay inside the owner's band to learn the response. They are labelled
  as learning steps.
- The twin is advisory, as [safety.md](../safety.md#planner-and-model-containment)
  requires. It never widens the band or a declared limit, and its prediction
  never counts as evidence. Only the measured temperature verifies a plan.

### 4. The planner

- **Go planner in Core first.** A dynamic program over the existing price,
  PV and weather horizon. Its state is the predicted zone temperature, and
  the actions are the declared control values, few by construction (for
  example seven offsets, or on/off). The cost is energy at the marginal
  price from `thermal.MarginalPrice` plus a penalty for distance from the
  target. The band and the `min_on_s`/`min_off_s` timings are hard
  constraints.
- **Zones are planned together.** Every enabled zone must stay inside its
  own band. A shared source is planned once for all the zones it serves, so
  the zone that needs the most heat sets it. Local controls then trim each
  room toward its own target, and the twin of each zone learns its response
  to both. A shared source is never raised beyond what keeps the warmest
  zone inside its band, unless that zone's local control can hold it back.
  When no value satisfies every zone, the plan is infeasible. The shared
  source is then released to the device and the conflict is shown to the
  owner, not hidden by favouring one room.
- **The planner works out the buffering itself.** It preheats before an
  expensive period exactly as far as the learned inertia lets the zone
  coast, and stores PV surplus as heat where the twin says it will last.
- **Energyplan can take over later** through the reserved `thermal_loads`
  and `thermal_state` fields, behind an explicit worker capability, as
  `hourly_import_caps` did. Core then validates its trajectory against the
  same twin and band and keeps the Go planner as fallback.
- `thermal.DecideIntent` stays as the Go rule for a zone whose twin is not
  yet trusted, or it is removed. See question 3.

### 5. Dispatch and ownership

- A heat block in the control tick, after the solar feed, on
  freshness-allowed ticks only. A stale site meter already defaults every
  driver.
- Write when the planned value changes, plus a refresh at the lease
  interval. Never faster than `min_interval_s`.
- One owner per driver. Operator holds and planned commands share the
  per-driver state now in `api`, moved to a package both can use. An
  operator hold wins. The planner treats the held control as fixed at its
  held value for every zone it serves, and plans the rest around it until
  the hold ends.
- Each slot records the planned value, the command, the driver result, the
  predicted temperature and the measured temperature. That is the evidence
  the product rules ask for, and it is what the twin learns from.

### 6. Safety

Each protection follows [safety.md](../safety.md#clamp-discipline):

| Condition | Limit | Reaction and recovery |
|---|---|---|
| Value outside the declared input | declared min/max | clamp (exists today) |
| Comfort reading stale or missing | the watchdog timeout | release to device; resume on a fresh reading |
| Measured temperature outside the band | owner band | release to device; resume back inside |
| Twin not trusted, or error over limit | confidence gate | release to device, back to shadow; resume when trusted again |
| Writes too frequent | `min_interval_s` | skip; the next due slot sends |
| Short cycling | `min_on_s`, `min_off_s` | hard constraint in the plan; Core refuses a value that breaks it |
| Core stops refreshing | lease interval | driver default (see question 2) |

The site-meter and watchdog paths stay ahead of all of these.

### Not in scope

Temperature schedules, the Live Energy node for heat pumps (#520, part 1),
the Settings form and an Energyplan thermal capability. Each is a separate
change.

## Open questions for Fredrik

1. **Order.** The roadmap lists bounded thermal control as *Later*. Should
   this go ahead now behind the opt-in, beginning with the twin in shadow,
   which controls nothing?
2. **Lease on process death.** A hold's timer lives in the FTW process, so
   a crash or power loss leaves the last value on the device. The same holds
   for today's operator hold. Should planned control require a device-side
   lease (for example a hub's own timed boost), accept a host lease with the
   risk stated, or limit planned values close to the device default?
3. **Untrusted twin.** Should a zone without a trusted twin get the simple
   `DecideIntent` rule, or no planned control at all?
4. **Learning steps.** May the planner make small steps inside the owner's
   band only to learn the response, labelled as such, or should it learn only
   from ordinary operation and owner holds?
5. **Zone conflicts.** When one shared source cannot keep every zone in its
   band, is releasing the source and telling the owner right, or should the
   owner be able to rank zones?
6. **First case.** The roadmap names a hot-water store. Heishamon's curve
   offset is the only declared heat control with hardware evidence today.
   Which should prove the path first?
