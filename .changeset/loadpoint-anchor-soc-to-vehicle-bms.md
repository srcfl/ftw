---
"forty-two-watts": patch
---

Loadpoint SoC now anchors to the live vehicle BMS reading when a vehicle
driver (e.g. Tesla via TeslaBLEProxy) is online and matched to the
loadpoint. Previously the EV card showed the delivered-Wh *inferred*
estimate labelled "(vehicle)", which drifts from the car's real SoC
(chargers like Easee can't read the pack) — so an actively charging
Tesla reading 31% could show e.g. 36% on the card. The control loop now
re-anchors `current_soc_pct` to the paired vehicle's BMS SoC each tick
(only when the reading is online, fresh, and not driver-flagged stale),
so the dashboard, the planner's `InitialSoCPct`, and the MPC all agree on
BMS ground truth. When the vehicle goes BLE-silent the estimate continues
from the last known BMS value instead of snapping back to the plug-in
guess.
