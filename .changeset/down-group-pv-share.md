---
"ftw": patch
---

Batteries now take the whole charge FTW asks for when another battery is down. On a site with inverter groups, the PV of an inverter whose battery is offline, faulted or cannot charge was still counted as charge for that battery, and that part of the target went to no battery. With 3.3 kW of PV on the down battery's inverter and a 9.6 kW charge target, the other two batteries got 6.3 kW in total; when the target was smaller than that PV, they got nothing and the surplus went to the grid. That PV now counts as power on the AC bus, so the other batteries share the whole target by capacity, within their limits.
