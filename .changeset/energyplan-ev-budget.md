---
"ftw": patch
---

Give the Energyplan planner 1.5 s instead of 0.5 s when a car is plugged in. On a Raspberry Pi 4, 0.5 s stopped just before the planner could improve on its first plan for the car, and the published plan could cost about 11% more than the best one. Battery-only plans keep their 0.5 s budget, and the first slot's remaining time still caps every budget.
