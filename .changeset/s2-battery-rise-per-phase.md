---
"ftw": patch
---

A battery charge rise now fits each phase before FTW sends it. The room is the fuse's working limit less the phase's reading and less FTW's own rises the meter does not show yet: the car's raises and the battery's earlier orders. The car's raise comes first. A battery order stays counted until the meter has had time to show the battery's own reading of it, so a battery that ramps slowly does not get a second rise on top. On a meter that reports phase currents without phase power, a phase counts as export only when the other phases cannot carry the exported total alone; any other phase counts as import. Near the line this keeps the battery from charging into a phase the meter shows full, and from stacking a rise on one the meter has not shown yet, which pushed a phase over the fuse and made the charge rise and fall around the limit. Holding a charge is never a rise, so this check never cuts a charge. A frozen meter is not covered here.
