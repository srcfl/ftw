---
"ftw": minor
---

The planner now plans each battery within what it has shown it can do. When
the battery model has learned that the inverter holds grid import while the
battery charges, or that the battery stops at a DC current or power, each
slot of the plan stays inside that limit, at the battery's temperature now.
At night the plan then charges over more hours at what the battery can
take, instead of expecting its full power and falling short every quarter.
Bundles Energyplan 0.9.0, which takes these per-slot limits; Core's own
planner holds them too. A battery with nothing learned plans as before.
