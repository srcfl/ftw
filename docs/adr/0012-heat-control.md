# ADR 0012: Heat control from a target temperature and a learned thermal twin

- Status: accepted.
- Implementation: the declaration
  ([#1995](https://github.com/srcfl/ftw/pull/1995)), the twin in shadow
  ([#1996](https://github.com/srcfl/ftw/pull/1996)) and the planner package
  (`go/internal/heatplan`, from
  [#2013](https://github.com/srcfl/ftw/pull/2013) in
  [#2022](https://github.com/srcfl/ftw/pull/2022)) are merged. Nothing
  calls the planner yet, and planned control has not shipped.
- Date: 2026-11-10
- Decided by: Fredrik on 10 November 2026, in his
  [answers on #1994](https://github.com/srcfl/ftw/pull/1994#issuecomment-6763083595),
  and on the hot-water floor on 12 November 2026
  ([answer](https://github.com/srcfl/ftw/pull/1994#issuecomment-6832196178)),
  and on comfort that follows the day's price on 15 November 2026.
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
      flex_c: 5.0        # the band 55-65 °C; see the hot-water floor below
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
- **A hot-water zone has a floor, with a short dip allowed.** For a zone
  whose comfort sensor is declared `asset = "dhw"`, measured at the top of
  the tank where water is drawn:
  - The band never goes below the zone's **floor**, whatever `target_c` and
    `flex_c` say.
  - After a draw, such as a shower, the planner may hold off reheating so it
    can wait for cheaper energy. The tank may then sit under the floor for a
    limited time, and never under a minimum.
  - The defaults are a **floor of 50 °C**, at most **2 hours** under it after
    a draw, and never under **45 °C**. The owner may choose other values,
    including ones outside these. An explicit choice by the owner wins, and
    Core does not reject it.
  - Core holds the owner's floor and timer, or the defaults when nothing is
    set. When either limit is reached, Core releases the tank to the device,
    which reheats by its own regulation. Core keeps the timer, so this does
    not depend on the layer.

  The reason is Legionella. It multiplies between about 20 and 45 °C, and
  the risk is water that stays lukewarm for days, not a dip of a few hours:
  industry sources give a doubling time of 2–6 hours at best, around
  37–42 °C in stagnant water with biofilm, and days to weeks to reach high
  counts in real systems. The Swedish building rules ask for at least 50 °C
  at the tap
  ([BFS 2024:8](https://www.boverket.se/sv/PBL-kunskapsbanken/regler-om-byggande/hygien-halsa-och-miljo/vatten-avlopp/legionella/),
  chapter 8, section 6), and Boverket recommends 60 °C in the water heater.
  German guidance for single-family houses says to avoid running below
  50 °C. The growth figures are industry sources, not measurements, so the
  defaults lean cautious. Fredrik set them on 12 November 2026; see
  [Decisions](#decisions).
- Without a comfort sensor, a zone cannot be planned. The roadmap does not
  let Core assume indoor temperature.

#### Comfort follows the day's price

Decided on 15 November 2026; see [Decisions](#decisions), point 9. What a
degree is worth to the owner depends on
what the day costs. A warm bathroom floor is cheap comfort on a cheap day,
and a slightly cooler room is an easy saving on a dear one. That is a choice
about comfort, not about timing, and it needs no model of the house. The
rule is in `heatplan.PriceComfort`; the configuration below is the shape it
will take, and comes with the change that acts on it:

```yaml
thermal:
  cheap_day_ore_kwh: 150      # at or below: all of cheap_day_raise_k
  normal_day_ore_kwh: 260     # the owner's own target
  dear_day_ore_kwh: 400       # at or above: all of dear_day_lower_k
  zones:
    - comfort_sensor: { driver: wiser, metric: bathroom_floor_c }
      cheap_day_raise_k: 2.5
    - comfort_sensor: { driver: wiser, metric: bathroom_air_c }
      dear_day_lower_k: 1.0
      step_k: 0.5
```

- The price is the mean total consumer price of the day the comfort is for,
  once at least 22 hours of it are known. Between the levels the target
  moves linearly, so a day one öre over a level is not a different day.
- Each direction is per zone and off at zero. A floor may follow cheap days
  up while the room air only follows dear days down.
- The moved target stays inside the zone's hard band.
- **Nothing has a default.** These numbers depend on the contract, the
  price area and the household. A site that sets none is unchanged.
- A shift rounded to the control's step never passes the owner's limit: a
  raise of 2.5 K in steps of 1 K is 2 K.
- **The comfort rule comes first.** It moves the target, and the plan and
  the baseline it is measured against both stand on that target, so the
  plan is never credited with what the comfort rule saved.
- **It is the owner's target, not planned control.** It needs no trusted
  twin. It still sends a value to the device, so decision 2 holds: a
  device-side timed hold, or a small step from the device's own value.
- **There is no least saving.** A limit on what the plans must save before
  they act was proposed and is not taken. In the one house that ran it,
  with 5 kr a day, the plans saved 0.1–4.9 kr and the limit was never
  reached, so it only turned the planner off. Instead the planner runs in
  shadow first and the owner sees what it would have saved per day, then
  turns planned control on with that number in view.

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
  Two holds seen on hardware: a Wiser hub's room override sent with
  `DurationMinutes` ends on a quarter hour and returns the room to its
  manual setpoint or schedule by itself, and CTC's 1000-series registers
  reset five minutes after the last write.
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
- **A zone's neighbours.** A zone inside a heated house loses much of its
  heat to the zones around it, not straight outdoors. A tiled bathroom
  with electric floor heating sits between rooms the heat pump keeps warm.
  Fitted against `T_out` alone, its `UA` is an effective value that holds
  only while the neighbours stay as they are; warming the house lowers
  the zone's need. Where a neighbouring zone's temperature is declared,
  the twin uses it as a second boundary.
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
  A released source stays with the device for at least 1 hour, and is
  taken back only when a plan keeps every zone inside its band for that
  whole hour. The owner is told once per conflict, not once per slot. A
  conflict that comes and goes therefore costs at most one release and one
  reclaim an hour, never one per slot.
- **Cost per kWh of heat, not of electricity.** Each heat input is
  costed as electricity at the slot's price divided by its efficiency: 1
  for direct electric heat, the learned COP at that outdoor and supply
  temperature for a heat pump. Storing heat in a cheap slot pays only when
  the price ratio beats the efficiency ratio plus the extra loss. A tiled
  floor charged with direct electric heat (COP 1) in a house that a heat
  pump also heats (COP 3-4 at 4 °C outdoor) rarely pays, even at a price
  spread of 1.5; the same hours spent raising the heat pump's flow can.
  When two sources can warm one zone, the planner prefers the cheaper heat
  per kWh, not the cheaper electricity.
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
| Core | Hot-water zone under its floor too long or too far | the owner's floor, timer and minimum; by default 2 h under 50 °C, or under 45 °C | release the tank to the device; resume above the floor |
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
the Settings form, an Energyplan thermal capability and a periodic pass to
60 °C or more against Legionella. Each is a separate change.

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

Fredrik answered on the hot-water floor on 12 November 2026:

8. **Hot-water floor.** A 50 °C floor, at most 2 hours under it after a
   draw, and never under 45 °C are the defaults. The owner may choose other
   values, including ones outside these; an explicit choice by the owner
   wins and Core does not reject it. Core holds the owner's floor and timer,
   with these three numbers as defaults when nothing is set.

Fredrik answered on comfort that follows the day's price on 15 November 2026:

9. **Comfort that follows the day's price** is taken, as section 1
   describes, as the owner's own rule. It needs no trusted twin, and
   decision 2 applies to what it sends. **There is no least daily saving.**
   The planner runs in shadow first, and the box shows the owner what it
   would have saved before planned control is turned on.

## Open

- **What makes a day cheap or dear.** Comfort that follows the price uses
  the day's mean against the owner's fixed levels. Whether a longer
  reference, such as the month, serves better is open until it has run in
  more than one house.
- **A least saving, as the owner's own number.** Decision 9 did not take
  one, on the evidence that in the one house that ran it the plans saved
  0.1–4.9 kr a day against a limit of 5 kr and the limit never opened. That
  figure was one room's saving read against a limit that is the household's.
  Summed over the three rooms, as the limit is, the same house's log reached
  5 kr on 15 and 16 November and held it open for 84 % and 41 % of the
  time. Part of that was a room model that overstated what a preheat saves;
  with the model corrected the sum is 2–4 kr a day and the limit is mostly
  closed, which is the effect decision 9 describes. The question left is what
  the limit is for. Proposed: not a switch that turns the planner off, but the
  owner's own answer to "what must a day's saving be worth to me before I
  accept a change of comfort or a write to my devices". It is off at zero,
  has no default, and the shadow run's saving per day is what the owner sets
  it against. `heatplan.SavingsGate` carries the rule so the owner's number
  has somewhere to go if the decision is revisited; nothing calls it yet.
- **Sitting out a short price peak.** The same house keeps a second rule of
  its own: a slot is a peak when it costs at least 200 öre and three times
  the lowest price of the next 12 hours, and a zone may sit at the bottom of
  its band for at most 3 hours of one. On a dear but flat day nothing is a
  peak and the rule does nothing, which is why it cannot replace the
  planner. `heatplan.PeakRelief` carries it as the owner's numbers, with no
  default, applied to a zone's target after `PriceComfort`; the band is not
  moved.
- **Holding off a reheat needs a control.** Many heat pumps start a
  hot-water charge by themselves at their own start temperature. The planner
  can only delay a reheat on a device whose driver declares a control that
  blocks or lowers the charge.
- **Where the efficiency comes from.** Proposed: a control declares its
  heat source (`direct_electric` or `heat_pump`); a heat pump's COP is
  learned from its metered electrical input where a driver reports one,
  and otherwise taken from a declared curve. Some controllers, such as the
  CTC EcoZenith, report no compressor power, so the curve is all there is.
- **How long a released shared source stays released.** Proposed: at least
  1 hour, reclaimed only when a plan keeps every zone in its band for that
  hour. The hour is a starting value to tune on hardware.
- **Heat inside the site's energy plan.** Proposed on 14 November 2026 and
  not yet answered by Fredrik. Sections 4 and 6 say how heat is planned
  and released, but not how it stands to the plan for the battery, the
  solar and the car. Proposed:
  - **The site's plan comes first.** Heat is planned against it and never
    asks it to change. Heat is costed at the import price. Surplus is only
    what the plan still exports after the battery and the car, costed at
    what it would have earned. Headroom is only what the plan leaves under
    the import limit.
  - **The import limit outranks the comfort band.** The planner lets a
    zone leave its band before it proposes a value whose predicted draw
    passes the limit, and reports the excursion. This changes one row in
    section 6: a zone outside its band is today released to the device,
    which then heats through the limit by itself. Proposed: when the limit
    is what holds the zone outside its band, FTW keeps the control and
    tells the owner. The hot-water floor is the exception. It is a health
    limit, Core owns it, and it still releases the tank.
  - **A battery holding energy does not make a slot cheap.** The battery
    follows the site's load, so heat moved into a slot where it discharges
    empties it earlier. Heat is therefore costed at the import price there.
    The one exception is energy the battery was to spend in a cheaper slot
    later in the horizon: heat may take up to half of it, at that later
    slot's price.
  - **The site's plan sees heat as known load, and the load model does not
    learn the moved heat.** Household load is learned as import − export +
    PV − battery − car. Heat is in it, so heat that moves with the price
    becomes noise in the learned week, and the battery is planned for
    where the heat was yesterday. Proposed: the layer reports each zone's
    planned electricity, and the difference from the zone left alone is
    taken out before [`loadmodel`](../../go/internal/loadmodel/model.go)
    learns, as the car's is. This touches the load model and the plan's
    inputs, so it needs the owner's decision.
  - **Zones share the site in the order they are planned.** A zone gets
    the surplus, headroom and battery energy the zones before it left. This
    is less than section 4 asks: zones on a shared source are not yet
    planned together.

  The battery rule comes from a simulation with synthetic houses and
  prices, not from hardware. A planner that took battery energy for cheap
  cost up to 23 SEK more per day than one that used the import price, and
  emptied the battery before the price peak ended. The rule above was
  never worse than the import price and at most 1 SEK per day better.
  With a battery large enough to cover every dear slot, moving a small
  load such as a hot-water tank cost 1–3 SEK per day more than leaving it
  alone, under every rule: only the site's plan knows what its stored
  energy is worth. Giving that plan the heat as known load is what
  answers this case.
