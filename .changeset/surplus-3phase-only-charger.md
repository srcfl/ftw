---
"forty-two-watts": patch
---

Fix surplus-only EV charging never starting on 3-phase-only chargers (e.g.
CTEK Chargestorm — 3Φ, 6 A minimum, no phase-switch register). The surplus
controller's 1Φ fallback assumed every charger can trickle on a single phase
(true for Easee, false for CTEK): on any day the PV forecast couldn't sustain
the 3Φ minimum it locked the loadpoint to 1Φ for the day and handed the
charger a ~1380 W offer, which a 3Φ-only unit can only answer by writing 0 A —
so it never charged. `pickSurplusSteps` now keeps the 3Φ-only step set and
never commits the day-long 1Φ lock when the loadpoint is pinned to
`phase_mode: "3p"`, and `resolvePhaseMode` no longer lets a stale 1Φ lock
override an explicit `"3p"`. A 3Φ-only charger now pauses cleanly below the
~4.1 kW floor and charges in 3Φ steps above it. Configure such chargers with
`phase_mode: "3p"`, `min_charge_w: 4140`, and 3Φ-only `allowed_steps_w`.
