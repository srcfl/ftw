# Thermal assets

Status: design foundation.

forty-two-watts should treat heat pumps, cooling, domestic hot water, and
buffer tanks as thermal flexibility, not as plain on/off loads. The central
rule is:

```text
42W owns the site-level economic intent.
The device driver owns the vendor-specific influence method.
```

That split is important because many heat pumps already have competent
internal control. A NIBE-class system should usually keep control of
compressor cadence, degree minutes, defrost, circulation, and comfort logic.
42W should bias it with a synthetic price signal. A simpler or more open
system can instead be biased with curve offsets, setpoints, SG-ready modes,
or relays.

## Site-level model

A thermal asset exposes:

| Field | Meaning |
|---|---|
| `kind` | `space_heat`, `cooling`, `dhw`, or `buffer` |
| `driver` | Lua driver name that can observe and influence the device |
| `strategy` | How the driver can be influenced |
| `comfort` | Hard min/max bounds for indoor or zone temperature |
| `storage` | Tank or buffer min/normal/max temperature |
| `limits` | Minimum run/idle time, max offset, max setpoint change |
| `state` | Current temperatures, electrical power, compressor state, mode |

The planner should reason in terms of thermal state of charge:

```text
space heating SoC ~= current indoor temperature within comfort band
DHW SoC           ~= tank temperature within safe band
cooling SoC       ~= inverse indoor temperature within comfort band
buffer SoC        ~= buffer temperature within configured band
```

The exact energy conversion from degrees C to kWh is learned over time. The
first implementation can still make useful decisions from headroom alone:
"there is room to preheat", "there is no room to shed", "DHW is below the
floor", etc.

## Influence strategies

Drivers advertise one or more influence surfaces:

| Strategy | Use when | Driver action |
|---|---|---|
| `synthetic_price` | Device has good native smart-price control | Publish 42W marginal price/tariff curve |
| `curve_offset` | Device exposes heating curve offset | Map preheat/shed to offset steps |
| `setpoint` | Device exposes target temperatures | Move target within hard bounds |
| `discrete_mode` | Device exposes SG-ready, eco/boost, or relay modes | Select normal/boost/block/eco |

The planner must not contain vendor protocol details. It emits intent:

```text
neutral
precondition    # preheat or precool inside comfort band
shed            # reduce thermal load while safe
protect_comfort # override economics to protect min/max bounds
boost_dhw       # fill DHW/buffer when cheap or PV surplus exists
```

The driver maps that intent to the configured influence strategy.

## Synthetic price

For smart systems, the preferred signal is a 42W marginal price:

```text
synthetic_price =
  marginal grid energy price
  + import / fuse pressure penalty
  + battery opportunity cost
  - thermal urgency credit
```

If live or forecast PV surplus covers the next thermal kWh, the marginal
grid energy price is the export value instead of the import price. That lets
the heat pump see "cheap" energy when the alternative is exporting PV, while
still preserving native device control.

Examples:

- NIBE: publish a synthetic price or tariff curve and let the heat pump
  decide how aggressively to react.
- Panasonic Aquarea via Heishamon: convert intent to zone curve-offset
  steps, and optionally DHW target/boost.
- Simple DHW heater: convert intent to discrete enable/boost/block.

## Planner logic

Each planning slot should:

1. Compute site marginal price from spot price, tariffs, PV export value,
   battery opportunity cost, and fuse/import pressure.
2. Compute thermal headroom from current temperatures and hard bounds.
3. Classify the slot intent:
   - protect comfort if a hard bound is near or already violated
   - precondition when energy is cheap or PV would otherwise export
   - shed when energy is expensive and there is thermal headroom
   - boost DHW when cheap/PV surplus and the tank is below max
   - neutral otherwise
4. Let the driver translate intent to its influence strategy.
5. Observe actual response and update the thermal model.

Comfort, legionella safety, frost protection, vendor fail-safe modes, and
minimum run/idle constraints are hard constraints. Price optimization is a
soft objective.

## Rollout phases

1. Contract package and documentation.
2. Observe-only drivers for NIBE/myUplink and Panasonic/Heishamon.
3. Policy controller using cheap/expensive/PV-surplus thresholds.
4. MPC participant that estimates thermal kWh and co-optimizes with
   battery, EV, and V2X flexibility.

## Example config shape

This is intentionally not wired yet; it documents the target shape.

```yaml
thermal:
  assets:
    - name: nibe
      driver: nibe
      kind: space_heat
      strategy: synthetic_price
      comfort:
        min_c: 20.0
        normal_c: 21.0
        max_c: 22.5
      dhw:
        min_c: 45
        normal_c: 50
        max_c: 58

    - name: aquarea
      driver: panasonic_aquarea
      kind: space_heat
      strategy: curve_offset
      comfort:
        min_c: 20.0
        normal_c: 21.0
        max_c: 22.5
      curve_offset:
        min: -3
        neutral: 0
        max: 3
```

