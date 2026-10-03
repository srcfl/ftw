---
"ftw": patch
---

Update the bundled Energyplan planner to 0.5.0. On a small box such as a Raspberry Pi 4, plans with a car get better within the 0.5 s budget: time that the solver's LP phases cannot use now goes to improving the plan Core gets. A car that cannot charge is planned with the exact battery method, and a car a fraction of a watt-hour short of its goal no longer restarts the charger. The planner keeps a valid plan when its solver fails, reports `gap_satisfied` when it meets Core's gap, fixes a wrong demand-charge sign for the running hour, and closes two ways it could report an unproven optimum. Core still validates every plan.
