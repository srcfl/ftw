# Clamp battery dispatch against the live meter

## Objective

After the plan's battery target is read but before dispatch, clamp the
target against the *current* grid meter reading so the battery cannot
discharge into the grid (or charge from the grid) when the load twin's
prediction was wrong.

Plan decides whether to charge/discharge. Meter decides how much.

## Original Request

> Discharges battery into grid — Load twin predicted 5.4 kW, actual was
> 2.1 kW. Battery discharged at -5 kW, exported ~3 kW.
>
> Root cause: The dispatch loop uses the plan's battery target as a
> setpoint without clamping against the live meter.
>
> Proposed fix: Add a real-time meter clamp after the plan target is
> read, before dispatch:
>
> - if charging (target > 0): cap at current export (don't import to
>   charge)
> - if discharging (target < 0): cap at current import (don't export
>   from battery)
>
> Plan decides whether to charge/discharge. Meter decides how much.
>
> Deadband: use existing `GridToleranceW` (same as the PI deadband).
> The code already uses 100W as the idle threshold in
> `applyPlanSignFloor`.

## Intake Summary

- Input shape: `existing_plan`
- Audience: site operators / field testers seeing unintended grid
  export caused by load over-prediction.
- Authority: `approved` (owner-described fix in their own product).
- Proof type: `test` + `artifact` — a unit test that exercises the
  clamp under over- and under-prediction, plus the actual code patch.
- Completion proof:
  1. A new test in `go/internal/control/` (or wherever dispatch lives)
     simulating load-twin over-prediction: plan says discharge −5 kW,
     meter reports +2 kW import → clamped to no worse than −2 kW so
     export stays inside `GridToleranceW`.
  2. The mirror case for under-prediction / charging: plan says charge
     +5 kW, meter reports +1 kW import → clamped to no worse than
     +1 kW so import doesn't grow.
  3. Existing dispatch tests still pass; no regression in the PI loop,
     slew, fuse guard, or stale-meter guard.
- Likely misfire:
  - Clamping above the PI/slew/fuse-guard layer and double-counting an
    existing protection — `docs/clamping.md` already lists seven
    clamps; this must be a *new* clamp with a quantifiable risk, not a
    duplicate of one already there.
  - Reading the meter at the wrong sign (the driver layer is the only
    place sign conversion happens; above it is site convention,
    positive into the site).
  - Applying the clamp when the meter is stale — the stale-meter guard
    already short-circuits dispatch in that case; the new clamp must
    not bypass it.
  - Clamping into a feedback loop with the PI (the saturation-curve
    bug story in `docs/clamping.md`).
- Blind spots considered:
  - Where exactly the plan target meets dispatch — `control/` vs.
    `cmd/forty-two-watts/main.go` ticker — needs Scout to confirm
    before Worker touches it.
  - Whether `applyPlanSignFloor` is the right hook, sits in dispatch
    or in the planner, and whether `GridToleranceW` is read from
    config or a constant.
  - Multi-battery dispatch: if there are multiple batteries, the
    clamp must operate on aggregate dispatch or be applied per
    battery in a way that respects priorities.
  - PV — PV is uncontrollable (curtailment aside); the clamp should
    treat current PV generation as a fixed term in the meter
    equation.
- Existing plan facts:
  - Add the clamp *after the plan target is read, before dispatch*.
  - Use `GridToleranceW` as the deadband.
  - `applyPlanSignFloor` already uses 100 W as an idle threshold —
    keep that interaction sensible.
  - If target > 0 (charge): cap at current export.
  - If target < 0 (discharge): cap at current import.

## Goal Kind

`existing_plan`

## Current Tranche

Validate the proposed clamp against actual dispatch code, choose the
exact insertion point and the precise formula in site-convention
signs, implement it behind tests, and audit that it does not double up
on an existing clamp or interact badly with the PI loop / slew / fuse
guard / stale-meter guard.

## Non-Negotiable Constraints

- Edge control latency stays <200 ms (CLAUDE.md core constraint).
- Site convention: positive W = into the site across the grid meter.
  Driver layer is the only place sign conversion happens — above it,
  everything is site convention.
- Every clamp must protect against a *quantifiable* risk
  (`docs/clamping.md`).
- Do not bypass the stale-meter guard or the watchdog.
- Tests must run under `make test`; new test must be deterministic
  (no real meter, use sim or table-driven inputs).
- No silent error handling — surface clamp activations via existing
  logging/metrics so they're visible in operation.

## Stop Rule

Stop only when:

- A test demonstrates the over-prediction → no grid export case.
- A test demonstrates the under-prediction → no grid import case.
- Existing tests pass.
- A final Judge audit confirms the fix maps to the original incident
  and does not duplicate or undermine an existing clamp.

## Canonical Board

Machine truth lives at:

`docs/goals/dispatch-meter-clamp/state.yaml`

If this charter and `state.yaml` disagree, `state.yaml` wins.

## Run Command

```text
/goal Follow docs/goals/dispatch-meter-clamp/goal.md.
```
