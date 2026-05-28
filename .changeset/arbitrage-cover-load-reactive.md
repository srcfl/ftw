---
"forty-two-watts": minor
---

**Fix: `planner_arbitrage` cover-load discharge slots now chase the live
zero-grid line instead of rigidly running the planned discharge power.**
When the DP picks a discharge slot to *offset expensive import* (rather
than to *export at peak price*), the energy-allocation path used to lock
the battery at `remainingWh × 3600 / remainingS` regardless of live
conditions — exporting at spot price any forecast-load undershoot and
under-covering any forecast-load overshoot. The EMS now routes these
slots through reactive PI on grid=0, the same path
`planner_passive_arbitrage` non-charge slots and `planner_self`
participant slots already use.

Detection: `PlannedGridW > -100 W` (no significant planned export) AND
`BatteryEnergyWh < -50 Wh` (discharge planned). Peak-export slots
(`PlannedGridW < -100 W`) stay on the energy path — extra export there is
bonus revenue at the price the DP picked the slot for. Charge slots
stay on the energy path so deliberate grid-charge intent is honoured.

Found 2026-05-28: plan estimated baseload 1.7 kW for a slot that scheduled
the battery to be empty by 23:30. Real load was 0.9 kW; battery sat at
-1.7 kW exporting 800 W at spot. Then load surged to 3.2 kW and the
battery stayed at -1.7 kW, forcing 0.5 kW import. Both directions are
now reactive — the slot's Wh budget guides where the battery is
generally headed, the meter decides the instantaneous power.
