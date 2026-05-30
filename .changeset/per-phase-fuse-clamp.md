---
"forty-two-watts": patch
---

fix(loadpoint): reactive per-phase fuse clamp for the EV charger

The site-level fuse guard only protects the three-phase *total* — a single
phase can still trip from house-load imbalance (a vacuum, kettle or oven on
one leg) stacked on top of the EV's per-phase draw, which forced manual
ramp-downs in the Tesla app. The loadpoint now reads the site meter's live
per-phase currents (`meter_l1_a/l2_a/l3_a`) and reactively caps the EV's
`max_amps_per_phase`: the worst phase drops by the full overage the instant
it nears the breaker, and recovers at 1 A/tick once there is headroom
(fast-down / slow-up servo, deadband below the limit). Pure, table-tested
`nextFusePhaseCapA`; clamp disabled cleanly when per-phase telemetry is
absent.
