---
"forty-two-watts": patch
---

**Codex review follow-ups for v0.107.0** — fixes 2 P1 and 2 P2 review
findings on the dispatch / loadmodel changes shipped in v0.107.0.

**P1: Heating coefficient survives restarts.** `main.go` had been calling
`loadSvc.SetHeatingCoef(cfg.Weather.HeatingWPerDegC)` at startup, which
unconditionally overwrote any value persisted from previous training.
After every binary update the adaptive fit was thrown away. New
`SeedHeatingCoef(w)` only writes the value when the model has no samples
yet — operator config is the cold-start prior, observation drives the
value once learning has begun. `SetHeatingCoef` remains for explicit
operator overrides.

**P1: Cover-load carve-out actually chases grid=0.** The PR #378
carve-out only set `useEnergyPath = false`; in production `main.go` wires
both `SlotDirective` and `PlanTarget`, so the code fell into the legacy
`!useEnergyPath` branch and called `SetGridTarget(plannedImportW)` —
chasing the planned positive import instead of grid=0. Result: cover-
load slot with a 1.7 kW planned import would back the battery off all
the way to idle instead of covering live load. Fixed by forcing
`SetGridTarget(0)` for carve-out slots and skipping the legacy
PlanTarget block when a carve-out predicate fires.

**P2: Live-export gate predicate tightened.** `passiveArbitrageIdleSlot`
used `dir.BatteryEnergyWh <= idleWhGate`, which is true for *any*
negative-energy slot (planned discharge). Tightened to
`|BatteryEnergyWh| ≤ idleWhGate` so the predicate names what it does
(true idle only). The planned-discharge case is now folded into
`coverLoadDischargeSlot`, which was also extended to cover
`planner_passive_arbitrage` (not just `planner_arbitrage`), and the
live-export gate now fires on either predicate.

**P2: SlotDeliveryStats catches sign mismatches.** Planned `-425 Wh`
discharge vs actual `+425 Wh` charge would have scored `|actual| /
|planned| = 1.0` = "on target" — the largest possible miss, invisible
on `/api/status`. New `SignMismatchCount` field fires when planned and
actual have opposite signs (and both exceed the idle cutoff). The
magnitude over/under counters then only fire on same-sign cases,
keeping their semantics clean.
