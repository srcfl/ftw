---
"ftw": patch
---

Update the bundled Energyplan planner to 0.7.0. With a peak-power tariff it now plans with the hour already in progress, so each day's peak counts once. A new plan is never worse for the car goals or the bill than the plan the planner already holds, a site whose limits cannot all be met gets a clear reason instead of a failed plan, and the planner needs less memory. The bundle now carries the Linux workers only; a Mac keeps Core's own planner. Core still validates every plan.
