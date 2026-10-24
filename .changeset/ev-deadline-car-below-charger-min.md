---
"ftw": patch
---

A car whose own charger takes less than the charger's minimum now charges toward its goal when no plan is available. Without a plan, FTW falls back to a reserve that runs the goal on its own. On a charger without a step table, that reserve gave such a car 0 W, for example a 3.7 kW plug-in hybrid on an 11 or 22 kW charger whose minimum is 4.1 kW. The reserve now orders the charger's minimum, as a plan does, and the car takes what it can.
