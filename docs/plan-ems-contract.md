# Plan / EMS contract

Design note for the dispatch-layer rework. Three principles, the mode
taxonomy they imply, the wire contract between the planner and the EMS,
and the migration plan.

**Status: shipped (2026-04-19), with a `planner_self` exception (2026-04-19,
issue #130).** Energy-allocation dispatch is the default for
`planner_cheap` and `planner_arbitrage`. `planner_self` is a reactive
self-consumption controller with a plan-driven idle gate — see the
"Exception" section below. The legacy PI-on-grid-target path remains as
`planner.legacy_dispatch: true` for emergency rollback (applies to
cheap/arbitrage only) and as the plan-stale fallback. See
`go/internal/control/dispatch.go` and `go/internal/mpc/service.go`
(`SlotDirectiveAt`).

## Motivating incident

Operator report (2026-04-17): plan said `grid_target_w = -51`, actual
grid was `-18`, battery charging ~3.9 kW during a high-price window.
Forecast had PV = 700 W; reality was 4.8 kW. Instead of exporting the
surplus at peak price, the battery absorbed it.

Root cause: the plan's per-slot output is a **grid target**, and the PI
loop chases it. When PV surprises on the upside, the PI sees excess
export and pulls the battery into charging to restore the grid target.
The plan's intent was "charge ~800 W from the forecasted PV" — but the
way that intent reaches the EMS is "drive grid to −51 W", which under
4.8 kW of PV means "absorb the surplus in the battery".

The plan would also need to re-decide whether to export once it sees
the real PV. Reactive replan exists but fires on integrated energy
error (500 Wh default), which at 4.1 kW surplus takes ~7 minutes —
well past the peak-price minute.

Two distinct failures. Fixing either without the other doesn't help.

## Three principles

### 1. The plan allocates *energy* per slot, not power

The DP already integrates `action_w × slot_duration` internally — that
product is what gets priced. Exposing power hides the conserved
quantity. Change the contract: per-slot output is
`battery_energy_wh` (signed — positive = charge, negative =
discharge), not `battery_w`.

Consequence: how fast the energy flows within a slot becomes a
tactical decision for the EMS, not a strategic decision for the plan.

### 2. The EMS converts energy to power in real time

Each tick:

```
remaining_wh  = slot.target_wh - delivered_wh_this_slot
remaining_s   = slot_end - now
battery_w     = remaining_wh * 3600 / remaining_s
```

Clamp against per-driver max charge/discharge power, SoC floor/ceiling,
and the site fuse. Split across multiple batteries by the active
mode's distribution rule (priority / weighted — unchanged).

Consequence: if reality runs ahead of plan (e.g. PV surprise fills the
energy target in 2 minutes), the EMS idles the battery for the rest of
the slot. If reality runs behind, the EMS accelerates.

### 3. The grid is the residual

Nothing in the control loop tracks a grid target. Grid flow is
whatever results from `load - pv - battery`. The PI loop on grid is
deleted from the plan-driven path.

Consequence: PV surprises flow to grid naturally. No feedforward
hack needed. The plan's intent is respected (the battery does what
it was told) and the grid sees whatever physics dictates.

This is the architectural flip. The PI still exists, but only for
**SoC tracking** against the plan's SoC trajectory — a slow inner
loop that corrects for model error, not a fast outer loop fighting
grid flow.

## What the plan still decides strategically

- How much energy each battery should absorb/release per slot
- Which slots to charge vs discharge (price arbitrage, SoC trajectory)
- When to trigger a full replan (passive divergence detector)

What the plan no longer commands directly:
- Instantaneous grid flow
- Instantaneous battery power

## Exception: planner_self

The "grid is the residual" principle assumes the planner mode *wants*
the battery to cross the zero-grid line on purpose (arbitrage, cheap
charge). `planner_self` doesn't — its contract is "never imports to
charge, never exports via the battery". It may discharge to cover local
import when the plan says this slot should participate.

Under pure energy-allocation dispatch that contract breaks as soon as
the forecast is wrong:

- Forecast said 5 kW PV, reality is 2 kW. Plan allocated `battery_energy_wh
  = +1000` for this slot. Dispatch commands +4 kW charge → grid imports
  2 kW to make up the shortfall. (Issue #130 — operator report 2026-04-19.)
- Forecast said 3 kW load, reality is 300 W. Plan allocated `−552 Wh`.
  Dispatch commands −2.2 kW discharge and pushes the site into battery
  export. Reactive self-consumption would have backed off at grid zero.
  (Symmetric failure — same bug, other direction.)

The DP enforces `ModeSelfConsumption`'s no-battery-export invariant only on
forecast. The EMS must enforce it on the live meter.

`planner_self` therefore executes as **reactive self-consumption with a
charge-only idle gate**:

- Participant slots use the PI loop to hold the live meter near 0 W:
  charge live surplus, or discharge to cover live import.
- Idle/charge slots floor negative battery targets to 0 so a stale
  discharging inverter is stopped instead of spending SoC.
- The plan contributes a per-slot **idle gate**: if
  `|SlotDirective.BatteryEnergyWh| / slot_hours < mpc.IdleGateThresholdW`
  the EMS refuses discharge for that slot — honouring the DP's decision
  to save SoC for later — but still absorbs true live PV surplus that
  would cross the site meter. "True surplus" is computed after removing
  current battery power from the meter reading, so a battery that is
  already discharging cannot create its own surplus and flip the slot
  into charge. Otherwise the battery participates using live
  self-consumption.
- When the plan is stale (`MaxPlanAge` exceeded) the idle gate is
  disabled and execution is indistinguishable from manual
  `self_consumption`.

The plan becomes a **participation schedule** for this mode, not a
power trajectory. The three principles still hold for the other planner
modes where they make sense.

## Mode taxonomy

Today's `Mode` is a soup of strategies, dispatch rules, and execution
states. Split into two orthogonal axes.

### Planner strategy (what the DP optimizes)

| Strategy | Objective |
|---|---|
| `self_consumption` | Minimize grid exchange for local use: charge PV surplus and discharge to cover load, without intentional battery export. |
| `arbitrage` | Maximize price spread. Cycle the battery when `export_price - import_price × 1/roundtrip_eff > threshold`. |
| `cheap_charge` | Charge from grid during cheap hours; discharge whenever. For low-PV, high-base-load sites. |

Exactly one selected at any time. Stored in `state.db` under `mode`,
exposed via `/api/mode`.

### EMS state (how the EMS acts on plan output)

| State | Behavior |
|---|---|
| `follow_plan` | Normal. Battery setpoint derived from `plan.battery_energy_wh` per §2. |
| `manual` | Operator sets `battery_w` directly via API. Timeout back to `follow_plan` after N minutes (so a forgotten manual command self-heals). |
| `idle` | No dispatch. Safety / maintenance. Explicit operator action. |
| `auto_fallback` | Plan is stale (> `MaxPlanAge`) or absent. Falls back to a local self-consumption rule. Not operator-selectable — entered automatically when the plan hook returns `(_, false)`. |

### Not modes — always on

- **Peak shaving** — hard constraint: grid import ≤ `fuse.max_amps × voltage × phases`. Fuse guard in `dispatch.go` already enforces this for any strategy.
- **SoC floor / ceiling** — per-battery parameters (`soc_min_pct`, `soc_max_pct`). DP respects them; dispatch clamps enforce them.
- **Backup reserve** — a parameter (`soc_reserve_pct`) that becomes a hard SoC floor when the operator wants outage resilience. Not a mode.
- **Multi-battery split** — internal dispatch rule (`priority` / `weighted`), not operator-facing.

## Wire contract

```go
// planner → EMS
type SlotDirective struct {
    SlotStart    time.Time
    SlotEnd      time.Time
    BatteryWh    float64  // signed: + = charge, - = discharge, total for slot
    SoCTargetPct float64  // plan's SoC trajectory at slot end (for inner PI)
    Strategy     Strategy // operator's selected strategy (echoed for logging)
}

// Service.SlotDirectiveAt(now) (SlotDirective, bool)
```

EMS holds the current directive, accumulates `delivered_wh` per slot,
resets when slot rolls over. On divergence the plan triggers a replan
with updated forecasts; the new plan may emit a different directive
for the remainder of the current slot (EMS resets accumulation to the
new directive's start).

### Divergence detector (replan trigger)

Replan remains reactive but with a clarified role: **detect strategic
drift, not tactical mismatch**. Triggers:

- `|integrated_pv_error_wh|` > threshold (existing, keep)
- `|integrated_load_error_wh|` > threshold (existing, keep)
- `|actual_soc_pct - plan_soc_target_pct|` > threshold (new — catches when the plan's SoC trajectory no longer matches reality)

Thresholds can be more lenient than today because the EMS is no longer
fighting reality between replans. Flapping risk drops.

## What changes in code

Required:

- `mpc.Service.SlotDirectiveAt(now)` replaces `GridTargetAt(now)`.
- `mpc.Plan.Actions[i]` exposes `BatteryEnergyWh` (derived from existing
  `BatteryW × slot_duration`) and `SoCTargetPct`.
- `control/dispatch.go`: plan-driven branch reads the directive,
  maintains `delivered_wh` state, computes `battery_w` per tick, drops
  the grid-target PI for this path.
- `control.State` gains `EMSState Enum` (`follow_plan` | `manual` |
  `idle` | `auto_fallback`).
- `/api/mode` responds with `{strategy, ems_state}` instead of a flat
  string. Existing flat-string mode field deprecated with one release
  of overlap.

Unchanged:

- DP internals (`mpc.go` Optimize / Bellman recursion).
- Per-battery split rules (`priority` / `weighted`).
- Fuse guard, SoC clamps, slew limiter in `dispatch.go`.
- Watchdog, stale-meter short-circuit.
- Forecast / price / twin services feeding DP inputs.

## Migration

Single PR. Small enough to review in one sitting and touches one layer
boundary cleanly.

1. Introduce `SlotDirective` alongside `GridTargetAt`. Both return for
   one release.
2. Switch `dispatch.go` plan branch to the new directive.
3. Remove `GridTargetAt` once no caller remains.
4. Update `docs/mpc-planner.md` + `docs/architecture.md` to reflect.
5. E2E test: reproduce the xorath scenario (forecast PV = 700 W,
   simulator PV = 4.8 kW, arbitrage mode, verify export > 3 kW within
   one control interval of the PV step).

No config schema changes. Mode persistence key (`mode` in `state.db`)
gets a companion key (`ems_state`) — both migrate in place.

## Open questions

1. **Inner SoC PI:** does the plan's SoC trajectory need to be a PI
   setpoint, or is the energy-accumulation bookkeeping enough? Probably
   enough on its own; the PI is a safety net for modeling error. Start
   without it; add later if divergence detector fires too often.

2. **Slot duration vs tick cadence:** today's 15-min slots + 5-s ticks
   give 180 ticks/slot. For a 200 Wh slot target, first-tick power is
   ~4 kW → typical batteries can do it. For tiny slot targets (say 20
   Wh), first-tick power falls below the battery's control resolution.
   Solution: clamp EMS minimum-effective-power and carry the residual
   to the next tick. Degenerate case only — flag as follow-up.

3. **Manual state timeout default:** 30 min? Operator preference. Start
   at 30, make configurable.

4. **`/api/mpc/diagnose` endpoint:** for ops to introspect plan
   decisions post-hoc. Returns for any historical slot: forecasts used,
   directive emitted, actual telemetry, replan reason if any. Separate
   PR, not blocking.

## Success criteria

- Xorath's scenario reproduced in an e2e test: forecast PV 700 W,
  actual 4800 W, arbitrage mode, expected export > 3 kW within the
  first tick after the step.
- `TestE2E_FullStack` keeps passing (the current failure on Erik's
  branch is orthogonal — Lua-driver warmup timing — and should be
  fixed separately).
- No regression in self-consumption mode (PV surplus still absorbed
  into battery until full, as today).
- Reactive replan frequency measurably lower in production telemetry
  after deploy (fewer spurious flips, tighter strategic fit).
