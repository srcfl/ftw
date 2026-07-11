---
"forty-two-watts": patch
---

Fix the Ferroamp battery under-charging (and spilling PV surplus to grid) on
multi-ESO sites where one battery is saturated. The EnergyHub splits a charge
setpoint evenly across all ESOs; a unit in CV taper near full — or held back by
the EHub's own SoC balancing — absorbs almost none of its share, so the pack
caps well below the commanded power (observed on Stefan's 4×ESO site: an 8.3 kW
command delivered only 4.3 kW, the rest exported).

Replaces the previous per-unit acceptance threshold (which flapped: a saturated
ESO trickling ~170 W against a ~650 W share still read "charging", dropping the
up-scale and re-spilling surplus) with a **delivery-ratio loop**: the driver
measures how much of its last on-wire setpoint the pack actually absorbed
(`eff = |delivered| / |on-wire|`, EMA-smoothed) and scales the next command by
`1/eff`, bounded by `MAX_DISPATCH_SCALE` (2.0×). A saturated unit's trickle is
simply summed in rather than voted on, so the command rises until the units
that *can* take more cover the deficit — converging on the commanded power
regardless of which units under-pull, with no threshold to flap on. The
SoC-capable count is retained only for the "every unit floored/ceilinged → idle"
guard. New `eso_accept_eff_x1000` metric exposes the measured efficiency.
Verified live: battery charge rose 4.3 kW → ~8 kW and the surplus stopped
spilling.
