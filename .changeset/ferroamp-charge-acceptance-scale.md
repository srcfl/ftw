---
"forty-two-watts": patch
---

Fix the Ferroamp battery under-charging (and spilling PV surplus to grid) when
one battery in a multi-ESO site is saturated. The EnergyHub splits a charge
setpoint evenly across all ESOs; an ESO in CV taper near full — or held back by
the EHub's own SoC balancing — accepts almost none of its share, so the pack
caps well below the commanded power. The driver's N_total/N_capable up-scale
only counted ESOs by SoC ceiling, so a unit saturated at a *mid-range* SoC
(observed on Stefan's 4×ESO site: 2×5.1 kWh units stuck at ~86 W while at 73 %
SoC) still counted as capable and the scale stayed 1.0 — an 8.3 kW command
delivered only 4.3 kW, the rest exported.

The charge-capability test is now **acceptance-based**: while charge is
commanded, an ESO drawing less than 100 W (debounced over 3 polls) is excluded
from `n_charge_capable`, so the existing up-scale (capped at 2.0×) redirects the
surplus to the units that can take it. Exclusion is debounced to avoid dropping
a unit during ramp; re-inclusion is immediate, so the moment a saturated unit
starts accepting again the multiplier drops and power redistributes evenly. New
`eso_charge_not_accepting` metric surfaces how many units are being skipped.
Verified live: battery charge rose 4.3 kW → ~8.7 kW and grid export of the
surplus stopped.
