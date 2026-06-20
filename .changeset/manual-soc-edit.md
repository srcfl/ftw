---
"forty-two-watts": patch
---

Manually correct a vehicle's State of Charge from the UI. The loadpoint
card (advanced mode) now shows an inline ✎ next to the SoC while the car
is plugged in — click it, type the real %, and it re-anchors the inferred
SoC via the existing POST /api/loadpoints/{id}/soc (then triggers an MPC
replan). Useful when there's no vehicle BMS reading and the inferred SoC
has drifted.
