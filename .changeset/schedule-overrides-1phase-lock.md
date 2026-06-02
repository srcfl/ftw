---
"forty-two-watts": patch
---

**A charge schedule now overrides the surplus 1-phase forecast lock.** On a
cloudy day the surplus_only logic can pin a loadpoint to 1-phase for the whole
day (`surplusLockedTo1P`, the "today's PV can't sustain 3Φ" verdict). That lock
is sticky and was applied even when the operator had set a **deadline-driven
charge schedule** that needs 3-phase grid power — so an "11 kW by 13:00"
schedule was silently throttled to ~3.7 kW (1-phase) and could miss its target.

Phase selection now puts an **active schedule first**: when a schedule SoC
target is set, the charger is given the operator's explicit phase pin or
`auto` (never forced to `1p` by the surplus optimisation), so a scheduled
charge can use 3-phase. With no schedule, the surplus 1-phase lock and the
30-minute near-term dwell verdict behave exactly as before. The precedence
lives in a single pure `resolvePhaseMode` helper with a table-driven test.
