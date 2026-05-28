---
"forty-two-watts": minor
---

feat(diagnostics): per-slot Wh delivery tracking for reactive dispatch paths

Adds an independent per-slot Wh accumulator that runs on every dispatch
tick regardless of which execution path was taken (planner_self, planner
passive_arbitrage idle slots, the planner_arbitrage cover-load carve-out
from PR #378, manual modes, plain self_consumption). At slot rollover
the actual fleet delivery is compared against the plan's
`BatteryEnergyWh`; ratios outside [0.5, 1.5] are logged and bump
`SlotDeliveryStats.OverDeliveryCount` / `UnderDeliveryCount`, surfaced
on `/api/status`. Idle slots (|planned| ≤ 50 Wh) are skipped — ratio
against ~0 is meaningless.

Pure observability — no dispatch decision reads the counters and no
hard Wh cap is applied to reactive paths. The point is to measure
first, decide on enforcement later.
