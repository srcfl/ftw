---
"forty-two-watts": patch
---

The manual SoC correction in the EV Charger modal is now a 0–100% slider
in whole-percent steps instead of a free-text number field, with a live
mono readout of the selected value. Easier to nudge on touch and removes
the fiddly decimal entry — the backend already rounds to the corrected
anchor.
