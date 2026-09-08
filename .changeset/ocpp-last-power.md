---
"ftw": patch
---

OCPP charger power uses one accept rule for dispatch and forecast, so a
stale, per-phase, negative, or energy-only sample cannot publish a phantom EV load.
1.6 Available/unplug now zeros last power like 2.0.1.
