---
"ftw": patch
---

The rule that a driver which cannot actuate leaves dispatch and gets its autonomous default now covers EV chargers and PV inverters, not just batteries. Until now only the storage loop filed what a driver made of its command: the PV curtail loop threw the error away and the loadpoint controller only logged it. So a wallbox that answered every poll and refused every setpoint held its last current while the plan kept booking the charge, and an inverter that refused every cap kept exporting into a negative price while the plan booked the saving — the same silent failure, one wire over. Which command counts on each path is chosen for what refusing it proves: the PV cap counts but the curtail release does not, and on the EV side the periodic setpoint counts while the safety standdown, the vehicle wake and the contactor cycle do not. A parked car refusing a wake, or a charger that has no pause action, is behaving correctly and stays in dispatch.
