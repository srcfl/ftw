---
"ftw": patch
---

The Charge now slider in the EV charger dialog now stops at the most FTW can order. It took its top from the charger's maximum, so a 32 A one-phase charger whose levels stop at 3.7 kW read 6–32 A, "32 A · 7.4 kW" and "FTW received 32 A" while FTW charged at 16 A. When a charger has a list of levels, the top is now the highest level at or below its maximum: 16 A · 3.7 kW for that charger.
