---
"forty-two-watts": patch
---

**Hardening: cover-load and passive-arbitrage-idle carve-outs now reset stale
energy-path bookkeeping on every tick they fire**, mirroring what
`preparePlannerSelf` already does for `planner_self`.

Without this, `slotDelivered` / `lastTickTs` / `currentDirective` could
carry leftover state from a prior energy-path tick into the carve-out
window. A subsequent transition back to the energy path within the same
15-min slot (e.g., a mid-slot replan flipping the slot's intent, or an
operator mode-hop) would then read those stale values and miscompute
`remainingWh`. Same forward-transition risk that `planner_self` has
guarded against since PR #131.

No new behaviour, no signal change in the steady-state cover-load reactive
path — purely defence-in-depth for plan-refinement / mode-transition
scenarios. Two regression tests pin the bookkeeping reset for both the
`planner_arbitrage` cover-load and the `planner_passive_arbitrage` idle
carve-outs.
