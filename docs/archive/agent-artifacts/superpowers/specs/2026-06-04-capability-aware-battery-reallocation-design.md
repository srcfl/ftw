# Capability-aware battery reallocation — design

**Date:** 2026-06-04
**Status:** implemented (TDD) on branch `worktree-battery-capability-reallocation` — `reallocated_w` metric deferred to fast-follow. See the "As built" notes inline.
**Area:** `go/internal/control` (dispatch), `go/internal/telemetry`, `go/internal/drivers`, `drivers/ferroamp.lua`

## Problem

On a multi-battery site, when one battery internally refuses a dispatch
direction (e.g. a Ferroamp ESO floored at its discharge SoC limit), the
unfulfilled share is **not** reallocated to a capable sibling. It leaks
straight to the grid.

### Live evidence (2026-06-04, Fredrik's site, 192.168.192.40)

Two batteries, mode `planner_passive_arbitrage`, morning low-PV:

| | Ferroamp | Sungrow |
|---|---|---|
| SoC | 0.14 | 0.075 |
| dispatch target | −901 W | −569 W |
| actual `bat_w` | **0 W** | −560 W |
| grid | **importing 903 W** | |

Ferroamp metrics: `eso_discharge_capable: 0`, `eso_dispatch_commanded_w: 0`.
The Ferroamp Lua driver's `DISCHARGE_FLOOR_SOC` (then 0.15) counted zero
ESOs capable at 0.14 SoC, so `driver_command` took its "all units floored
→ idle" branch and published 0 W. The Go dispatcher had allocated −901 W
to it regardless — it has no visibility into the driver's internal floor —
and never moved that deficit to the Sungrow, which was discharging at
7.5 % SoC and had ample headroom. Grid import ≈ the Ferroamp's
undelivered share.

`slot_delivery_stats.under_delivery_count` read 0 throughout — the
existing aggregate under-delivery accounting did not surface this.

### Two independent root causes

1. **Floor too high for the operator's intent.** Addressed separately and
   already shipped as a per-site config override:
   `discharge_floor_soc: 0.11` (live + saved on the Pi). Default stays
   0.15. See `2026-05-27-ferroamp-soc-bounds-config-design.md`. **Out of
   scope for this spec.**
2. **No cross-battery reallocation when a sibling is incapable.** This
   spec. The floor override reduces how often a battery floors, but the
   genuine floored case (any battery at its true limit) must still hand
   its share to a capable sibling rather than the grid.

## Goals

- When the site target is discharge and battery X cannot discharge right
  now, X is excluded from the discharge split and its share is
  redistributed proportionally across discharge-capable batteries, bounded
  by each one's `maxDischargeW`.
- Symmetric for charge: a battery at its charge ceiling is excluded from
  the charge split; its share goes to charge-capable siblings.
- The driver is the source of truth for "can I move this direction right
  now" — it already knows (Ferroamp: per-ESO floor/ceiling counts).
- When no capable battery can absorb the demand, the residual goes to the
  grid **intentionally and observably** (a metric), not as silent leakage.

## Non-goals (v1)

- **Headroom-aware allocation.** The signal is a boolean (capable / not),
  not "available watts before I hit my limit". Matches Ferroamp's discrete
  capable-count behaviour and solves the reported case. Continuous headroom
  is a possible v2.
- **`weighted` / `priority` legacy distributors.** v1 wires reallocation
  into `distributeProportional` only — the path all `planner_*` modes use.
  The legacy weighted/priority distributors are left unchanged (flagged as
  future work).
- Changing the planner/MPC. The plan still allocates aggregate energy; this
  is purely how the EMS distributes that aggregate across the fleet within
  one cycle.

## Design

### Part 1 — capability signal (driver → telemetry)

`host.emit("battery", {…})` gains two optional boolean fields:

```lua
host.emit("battery", { w = …, soc = …, discharge_capable = <bool>, charge_capable = <bool> })
```

- **Absent = unknown = assume capable** in that direction. Back-compatible:
  every existing driver that doesn't set them keeps today's behaviour.
- **As built — transport via `DerReading.Data`, not new typed fields.**
  `emitTelemetry` already passes the whole battery emit table to
  `Store.Update` as `data json.RawMessage`, so the flags ride along for
  free; the dispatcher parses them once per cycle when it builds
  `batteryInfo` (`control/dispatch.go` already does this for per-phase amps:
  `json.Unmarshal(r.Data, …)`). This replaced the original plan of adding
  `DischargeCapable/ChargeCapable *bool` to `DerReading` + extending the
  `Store.Update` signature — that signature has ~50 test call sites and the
  churn wasn't worth it when `Data` already carries the fields. No hot-path
  cost (parse is in the per-cycle control loop, not per-poll `Update`), and
  last-known preservation is unnecessary (the Ferroamp driver emits the
  flags every poll).
- `drivers/ferroamp.lua`: in `driver_poll`, set
  `discharge_capable = (n_discharge_capable > 0)` and
  `charge_capable = (n_charge_capable > 0)` on the battery emit. The
  counts already exist in `driver_poll`'s per-ESO floor/ceiling loop
  (`last_eso_discharge_capable` / `last_eso_charge_capable`). No new
  hardware read.

### Part 2 — reallocation (dispatcher)

`go/internal/control/dispatch.go`:

- `batteryInfo` gains `dischargeBlocked bool` and `chargeBlocked bool`
  (as built: **"blocked", not "capable"**, so the Go zero value `false` =
  not blocked = capable is the safe default — a directly-constructed
  `batteryInfo` and any non-reporting driver stay capable). Populated by
  `batteryDirectionBlocks(r.Data)`, which flags a direction blocked only
  when the driver explicitly emits `*_capable: false`.
- `distributeProportional` splits the **total desired** site battery power
  (unchanged: total, not delta — keep the existing "don't distribute from
  the delta" invariant). New step: when the desired aggregate is discharge,
  partition online batteries into capable / incapable for that direction.
  Distribute only across capable batteries, proportional to capacity (and
  the existing `groupPV` locality bonus still composes). Each battery is
  still bounded by its `maxDischargeW` (`clampWithSoC` / `DriverLimits`
  downstream). Incapable batteries get target 0 for that direction.
  Symmetric when the desired aggregate is charge.
- If the capable set is empty (every battery blocked in the demanded
  direction), **as built every battery is parked at 0** rather than falling
  back to the plain split. The grid outcome is identical (the residual
  imports/exports either way), but the dispatcher no longer commands a
  setpoint every driver would only idle — the target now reflects reality.
  This was a TDD-driven refinement of the original "fall back to today's
  behaviour" plan.

### Part 3 — safety + observability

- Reallocation runs **before** slew and the fuse guard, so both still bound
  the result. Slew also damps any transient capability flap (a one-cycle
  false "incapable" can shift at most `SlewRateW` of power).
- New per-cycle metric `reallocated_w` (site-level): the magnitude of power
  moved off incapable batteries onto capable ones this cycle. 0 when no
  reallocation fired. Makes the behaviour visible and explains residual
  grid flow that the planner didn't intend. **As built: deferred to a
  fast-follow.** The `control` package emits no telemetry metrics today
  (it returns `[]DispatchTarget`; metric emission lives in `main.go`), so
  `reallocated_w` needs new plumbing that doesn't belong in this
  behaviour-focused PR. The core reallocation ships without it; the metric
  is tracked as a follow-up.

## Data flow

```
ferroamp.lua driver_poll
  → host.emit("battery", {…, discharge_capable=false, charge_capable=true})
    → lua.go parses fields
      → telemetry.Store.Update → DerReading{DischargeCapable:&false, ChargeCapable:&true} (last-known preserved)
        → control.ComputeDispatch builds batteryInfo{dischargeCapable:false, chargeCapable:true}
          → distributeProportional: exclude from discharge split, reallocate to capable siblings (≤ maxDischargeW)
            → slew → fuse guard → []DispatchTarget
```

## Edge cases

- **All batteries incapable in the demanded direction** → no reallocation;
  residual to grid; `reallocated_w` records the gap. Same physical outcome
  as today, now intentional + observable.
- **Capable siblings saturate `maxDischargeW`** before absorbing the full
  share → they cap, remainder to grid, recorded.
- **Direction must not flip.** A charge-capable-only battery is never
  forced to discharge to fill a discharge deficit, and vice-versa.
- **Transient flap** (driver briefly reports incapable) → bounded by slew;
  next cycle corrects. Last-known preservation avoids nil-driven flaps.
- **Single-battery site** → capable set is {that battery} or {} ; logic is
  a no-op relative to today.

## Testing

`go/internal/control/dispatch_test.go` (table-driven, via `ComputeDispatch`
per the package's "tests reach unexported distributors through
ComputeDispatch" convention):

1. Two batteries, site wants discharge, battery A `dischargeCapable=false`
   → A target 0, B absorbs A's share up to its `maxDischargeW`, residual (if
   any) to grid; `reallocated_w` > 0.
2. Symmetric charge case (A `chargeCapable=false` at ceiling).
3. Both incapable → no reallocation, residual recorded, no panic / no
   division-by-zero in the proportional split.
4. Capability absent (nil) on both → identical targets to today
   (back-compat regression guard).
5. Capable sibling saturates `maxDischargeW` → caps, remainder to grid.

`go/internal/telemetry/store_test.go`:

6. `Update` with `discharge_capable=false` then a later emit omitting it →
   value preserved (last-known), mirroring the SoC-preservation test.

`go/internal/drivers/lua_test.go`:

7. `host.emit("battery", {discharge_capable=false})` round-trips to the
   `DerReading` field.

Ferroamp driver: extend an existing `driver_poll` test (or add one) to
assert the emit carries `discharge_capable = (n_discharge_capable > 0)`.

## Rollout

- Pure additive, behind the "absent = capable" default → no behaviour
  change for any driver until it opts in by emitting the fields.
- Ferroamp opts in immediately (this PR). Sungrow and others can adopt
  later; until then they're always treated as capable (today's behaviour).
- Changeset: `minor` (new telemetry field + dispatcher behaviour, driver
  capability support).

## Open questions

- Should `reallocated_w` also feed the reactive-replan trigger (so a
  persistent reallocation nudges the MPC to stop planning discharge into a
  battery that keeps refusing)? Leaning **no** for v1 — the planner already
  re-plans on PV/load divergence, and threading capability into the MPC is
  a larger change. Flag for follow-up.
