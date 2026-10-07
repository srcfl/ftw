---
"ftw": patch
---

Update the bundled Energyplan planner to 0.5.1. Plans with a car get cheaper within the box's time budget: once the planner has met the car's charging goals, it plans the battery around that charging again instead of keeping the battery schedule its goal search left. On the home box that schedule cost up to about 10 kr in one plan. A car that needs less than two minimum charging runs gets one run at the cheapest time, and a need of at most half a run counts as met instead of starting the charger. Core still validates every plan.
