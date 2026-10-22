---
"ftw": patch
---

Planned charging and Charge now run on three phases. In 0.155.0 a loadpoint set to auto could charge a three-phase car on one phase all night: with an Easee step table that also lists one-phase levels, the planner spread the charge at 3.1–3.5 kW, which only one phase can deliver, and FTW switched the charger to one phase to follow it. On a 3×20 A site the car got about 3.3 kW instead of about 10 kW. One phase never helps the fuse: it puts all the current on one phase, caps the car near 3.7 kW, and an Easee has to pause to switch back. Auto now means three phases, and only charging from spare solar below the three-phase minimum (6 A on each phase, 4.14 kW at 230 V) uses one phase, with the same hold against flapping as before. The planner gets only steps the charger can run on three phases. A loadpoint set to 1p keeps its one-phase steps, and an unset phase mode is still the charger's own default.
