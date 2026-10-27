---
"ftw": patch
---

A charger set to three phases with a maximum below 6 A on each phase now charges. Three phases cannot run below 6 A each, 4.1 kW at 230 V. With a maximum of 3.8 kW, every planned slot went out as 0 W while the reason still said "plan", so the car did not charge and nothing said why. FTW now plans and orders the lowest three-phase level, 6 A on each phase, for planned charging, the deadline reserve, spare solar and Charge now. Where the phase mode allows one phase and the level fits, the order runs on one phase as before. Saving such a charger, and starting FTW with one, shows a warning with the fix. A Charge now request below 6 A on each phase of a charger set to three phases gets no order, and the status now says "Below 6 A a phase" instead of naming a charger limit or the main fuse. On a charger that can switch phases, such a request now runs on one phase.
