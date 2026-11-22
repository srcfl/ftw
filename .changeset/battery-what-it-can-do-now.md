---
"ftw": minor
---

Judge a battery against what it can do now, not only against FTW's order.

A home box with a Sungrow hybrid sent 23 "not following" notices in two
weeks, though its battery did all it could. At night the inverter held grid
import at about 4.3 kW, so the battery took 4.3 kW less the house load; on
discharge the battery gave a flat 22 A.

Core now learns these limits per battery from its own control journal. When
a battery falls short of its order in the right direction and the grid
import, grid export, battery current or battery power stays flat, Core keeps
that run. Once the same level shows at least three times on two days, a
shortfall at that level reads "At its usual limit", with words such as "The
inverter takes at most 4.3 kW from the grid. FTW has seen this 14 times since
8 Nov." A battery that gives well below its limit still warns, and so does
one that gives less than half of its order, however often: a broken battery
never becomes normal. A grid limit counts only while the battery moves and
the meter shows live, changing readings, so a meter driver that resends a
cached value teaches nothing. A new, lower limit becomes normal only after it
repeats on two days, and the old one stays on record. Limits are kept per
temperature band when the battery reports its temperature.

A battery whose driver reports `available_charge_W` or
`available_discharge_W` reads "Limited by the battery" at once when it gives
that much. A battery whose driver reports `device_state` standby or starting
still warns, now with that reason. These verdicts change the words and the
status only; dispatch, the fuse guard and commands do not read them. GET
/api/battery_models shows the learned limits, and POST
/api/battery_models/reset forgets them along with the rest of the model.
Limited verdicts never send the "not following" notice.
