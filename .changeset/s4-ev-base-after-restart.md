---
"ftw": patch
---

After a restart, the home battery no longer stays in discharge for a car the meter already shows charging. Until the charger's first order of the new run can have landed, the battery counts the car's measured draw as part of the meter reading, never above FTW's latest order to the car and never above the consumption the site meter shows. The draw counts only when the charger measured it within one control interval of the site reading and its last poll succeeded. Before, the 0 A orders FTW sent while the meter was still starting counted as the car's draw, so a lower order looked like a rise and the battery kept discharging toward its floor.
