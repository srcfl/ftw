---
"ftw": patch
---

After a restart, the home battery no longer stays in discharge for a car the meter already shows charging. Until the charger's first order of the new run can have landed, the battery counts the car's measured draw as part of the meter reading, never above FTW's latest order to the car and never above the consumption the site meter shows. FTW does not know the charger's offer in force then, so the battery still keeps room for the car to rise from its draw to the charger's max_charge_w; a charger with no max_charge_w gets no such credit. The draw counts only when the charger measured it within one control interval of the site reading and its last poll succeeded, so a charger that samples every 30 s or more rarely gets it. Before, the 0 A orders FTW sent while the meter was still starting counted as the car's draw, so a lower order looked like a rise and the battery kept discharging toward its floor.
