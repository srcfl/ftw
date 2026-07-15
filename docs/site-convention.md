# Site sign convention — boundary-view, imports positive

FTW uses a **unified sign convention** for all power and energy
values above the driver layer. Every telemetry field, API response, log
line, battery model, chart — all use the same signs.

## The rule (one sentence)

> **Positive W = energy flowing INTO the site across its boundary.**

The boundary is where the utility grid meter sits. Stand on the street side
of that meter and look inward. Anything that adds to what the meter is
reading (import) is **positive**. Anything that reduces it (or drives it
past zero into export) is **negative**.

```
         Utility grid
              │
              │  (+) import →
  ════════════╪══════════════  ← site boundary (grid meter)
              │  ← export (−)
              │
 ┌────────────┴────────────────────────────┐
 │                                         │
 │   ↑  load consumes (makes meter go +)   │
 │   ↑  battery charging (load, makes + )  │
 │   ↓  PV generates (pushes TO site, −)   │
 │   ↓  battery discharging (sources, −)   │
 │                                         │
 └─────────────────────────────────────────┘
```

The mental model: the sign tells you what the **grid meter** would show if
this one thing were the only thing running.

## Signs per DER type

| DER | `+ W` means | `− W` means |
|---|---|---|
| **Grid meter** | importing (grid → site) | exporting (site → grid) |
| **PV** | (never positive) | generating (pushes to site → reduces import) |
| **Battery** | charging (load, adds to import) | discharging (source, reduces import) |
| **V2X charger** | charging vehicle (load, adds to import) | discharging vehicle (source, reduces import) |
| **Load** | (always positive) | — |

Balance equation (all signed):

```
grid_w = load_w + bat_w + pv_w
```

In self-consumption mode, the controller drives the site meter toward the
grid target (normally 0 W): it charges from live surplus and may discharge
to cover local load. It must not intentionally export via the battery; export
should come from PV unless an explicit export-capable strategy is selected.
Other modes such as peak-shaving, weighted target-following, and arbitrage
may still issue negative battery targets when their contract calls for
discharge.

## SI units everywhere

| Quantity | Unit | Notes |
|---|---|---|
| Power | W | Signed per above |
| Energy | Wh | Positive magnitude; direction in the field name (`import_wh`, `export_wh`, `charge_wh`, `discharge_wh`) |
| Time | s or ms | Explicit in the field name |
| Voltage | V | |
| Current | A | |
| Frequency | Hz | |
| Temperature | °C | |
| State of charge | fraction 0..1 | **Not percent** |

SI prefixes (k, M, m) only appear in the UI display layer. API and telemetry
always use the base SI unit.

## Where the sign flip happens

**At the driver boundary.** Each device speaks its own native convention:

- Ferroamp: `pbat > 0` = discharging (we flip: `w = -pbat`)
- Sungrow: unsigned magnitude + direction bit (we set `w = +mag` for charge, `-mag` for discharge)
- PV values from inverters: typically unsigned positive magnitude (we flip: `w = -magnitude`)
- A future OEM using whatever convention: driver translates.

The driver **MUST** translate to the site convention before calling
`host.emit(...)`. Likewise for commands: the controller sends
`power_w` in site convention, the driver translates to native commands.

This is the **only** place sign conversion happens. Everything above the
driver layer uses the site convention. No mental gymnastics.

## Dispatch examples

### Grid importing, mode allows reducing import

- Observed: `grid_w = +2000` (importing 2 kW)
- Target: reduce import in a mode that may discharge, such as peak-shaving
- Controller PI produces correction → battery should DISCHARGE
- Controller issues `driver_command({"action":"battery","power_w":-1500})`
- Driver translates:
  - Ferroamp: `{"cmd":"discharge","arg":1500}` over MQTT
  - Sungrow: regs 13051=1500, 13050=0xBB (discharge), 13049=2 (forced)
- Device responds, actual discharge ~ 1350 W (with typical 0.9 gain)
- Driver reads back, emits `{"type":"battery","w":-1350}`
- Controller sees `actual = -1350`, target was `-1500`, remaining error drives next cycle

### PV overproduction, want to charge battery

- Observed: `pv_w = -3000` (generating 3 kW, pushing to site)
- Observed: `load_w = +800`
- Result: `grid_w = -2200` (exporting 2.2 kW)
- Controller decides: charge battery with the surplus
- Issues `driver_command({"action":"battery","power_w":+2000})`
- Driver translates to native CHARGE command
- After lag, `bat_w = +1900`, grid moves toward 0

## Why this convention (over thermodynamics-first)

There's a competing convention where everything that feeds the bus is
positive (treating the site bus as a control volume, sources positive).
That's physically elegant but creates problems in practice:

1. **The grid meter is the customer's reality**. Bills are in kWh imported
   minus kWh exported. Having the app's sign match what the utility sees
   avoids a mental-flip every time you look at a number.
2. **Battery charging = load**. Charging IS a load from the site's point
   of view. It behaves like any other consumer on the bus.
3. **Existing integrations**. Home Assistant, most charts, most retail
   products all use "import positive" for grid, which also forces
   "generation negative" to keep the balance equation consistent.

So we pick: **grid-meter-positive, view the site from the boundary**.

## Verification

- Each driver's telemetry emission is covered by tests that assert the sign
  (e.g., `emit_pv` always produces `w <= 0`)
- Integration tests between Lua drivers and simulators verify
  that a `+N` charge command produces an actual reading with `bat_w > 0`
- The control loop's own tests assert both sides of the contract:
  self-consumption discharges on import to hold grid near zero, while planner
  idle/charge slots do not keep individual batteries discharging

Any driver that violates the convention breaks a test. The convention is
enforced, not just documented.
