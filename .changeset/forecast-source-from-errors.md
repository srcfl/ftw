---
"ftw": patch
---

The planner now takes each forecast signal from the source that measured better over the last week, once it has three days of scored hours. Before, it used Energyplan solar even while that model was new, and kept the older load model even when that model overshot by 2 kW at night.
