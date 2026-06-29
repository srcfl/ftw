---
"forty-two-watts": patch
---

EV Charger modal reworked. The controls are now split into three tabs —
**PV charging** (surplus-only toggle), **Manual** (amp slider +
Start/Stop), and **Scheduled** (current-SoC correction + target-SoC-by-
deadline schedule) — so each charging mode has its own space. The
current-SoC correction lives under Scheduled because it's a planning
input. Both SoC inputs (current correction and Target SoC) are now
0–100% sliders in whole-percent steps with a live mono % readout instead
of free-text number fields. The "Let battery cover EV" toggle stays as a
persistent footer. No backend changes.
