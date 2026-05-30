---
"forty-two-watts": patch
---

**planner_arbitrage: the battery now reactively covers a sudden load on a
charge-from-PV-surplus slot.** Previously, when the DP planned to "absorb PV
surplus" this slot (a charge slot with `PlannedGridW ≈ 0` — charge from PV,
not buy from the grid) and a large unforecast load came in, the battery sat
idle at 0 W while the house imported the deficit from the grid, waiting for the
slow reactive replan (60 s+ cooldown) to catch up. The existing PlannedGridW
soft cap correctly *backed the charge off* toward available PV, but floored at
0 and never flipped to discharge, so the battery never supported the load.

The soft cap's back-off may now go **negative (discharge)** on a
charge-from-PV-surplus slot, driving projected grid back toward the plan's
`PlannedGridW` (~0) — i.e. the battery covers the load the moment PV can't,
instead of importing. This is the charge-side mirror of the existing
discharge-slot cover-load carve-out.

Three dispatch rails were aligned through a single `coverLoadChargeSlot`
predicate so the discharge isn't undone downstream: the soft-cap floor,
`planHasNonDischargeIntent` (so `noSelfDischarge` doesn't re-clamp it), and the
plan/exec sign floor (so it isn't treated as a sign mismatch).

Scope is deliberately narrow and safe:
- Only `planner_arbitrage`, and only charge slots whose plan was **not** a
  deliberate grid-charge (`PlannedGridW` below the 100 W import band). A real
  grid-charge slot still floors at 0 — its refill intent is preserved.
- Normal sunny charge-from-surplus operation is unchanged (the cap only fires
  on a live import divergence; absorbing surplus is untouched).
- The SoC floor, fuse guard, and slew limiter still bound the discharge.

Does not change PV forecasting or any planner mode other than
`planner_arbitrage`.
