---
"forty-two-watts": patch
---

Fix surplus-only EV charging never starting on a "dumb" charger (CTEK and
other AC wallboxes that don't report the vehicle's BMS SoC). In
self-consumption mode the home-battery PI absorbs all PV surplus, so the EV
loadpoint sees nothing to claim. `EVSurplusOnlyReserveW` is supposed to hold
back export headroom for the EV, but `SurplusReserveW` only reserved a
bootstrap floor for a plugged-but-not-drawing EV when the vehicle's SoC was
known and below its limit — a dumb charger reports no SoC, so the reserve was
0, the battery took everything, and the EV could never bootstrap (chicken-and-
egg, observed live on Stefan's CTEK).

`SurplusReserveW` now reserves the loadpoint's `MinChargeW` (or the ramp
headroom) for a plugged, surplus-only, not-drawing EV **unless the car is known
to be full** (SoC known and at/above its charge limit). This prioritises PV
into the EV ahead of the home battery, as intended. Trade-off: a finished-but-
still-plugged car on a dumb charger holds the reserve (exports rather than
charging the home battery) until unplugged — surfacing the charger's own "done"
state into the loadpoint would let us skip that case too (follow-up). Smart
chargers/paired vehicles are unaffected: a car known to be full still reserves
nothing.
