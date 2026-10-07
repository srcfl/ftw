---
"ftw": patch
---

Show an EV departure miss only when it is more than half of one minimum charging run, about 155 Wh for a 4.14 kW charger. Energyplan treats a smaller need as met instead of starting the charger for it. A car that is charging when the plan starts still counts a miss above 1 Wh.
