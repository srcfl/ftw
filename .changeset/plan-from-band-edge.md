---
"ftw": patch
---

A battery that sits below its lowest allowed charge, or above its highest,
gets a plan again. Core now gives Energyplan the nearest allowed charge, as it
already did for its own planner. With a car in the plan, Energyplan had kept
such a battery idle for a whole day, even when cheap night power made
charging pay. If Energyplan fails in that state, Core now plans from that
charge, where its own planner can model the site, instead of keeping an old
plan.
