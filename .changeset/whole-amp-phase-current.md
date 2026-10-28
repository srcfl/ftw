---
"ftw": patch
---

Read the site meter's phase current from its phase power as well. Many P1 meters report phase current in whole amperes, so a phase could carry up to 1 A more than the meter showed, and the car's fuse limit let it stay there. Each phase now counts as at least its power over its measured voltage, when the meter reports both and the phase powers add up to its total. Meters that report no phase voltage read as before.
