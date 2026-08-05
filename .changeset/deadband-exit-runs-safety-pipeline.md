---
"ftw": patch
---

A site sitting inside the reactive deadband is now protected. The reactive control arm stops early when grid power is close enough to its target, and that exit walked away from the cycle before the fuse guard and the fuse-saver ran — so on a site whose grid error was small, no protection ran at all. Two things bind on numbers the deadband never reads. An operator peak ceiling below the plan's grid target: the plan asks for 8 kW of import, the meter delivers 8 kW, the error is zero, and every watt above the ceiling is billed at the peak tariff the operator set it to avoid. And a single phase over the breaker while the three-phase sum sits still — a single-phase EV charger on L1 against three-phase solar export. Both now force the battery to bridge, the same way idle mode and the dispatch holdoff already did. A deadband tick with nothing binding still commands nothing.
