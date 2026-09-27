---
"ftw": minor
---

The `priority` and `weighted` control modes are removed. Priority never regulated, because nothing set its battery order: it held each battery at its measured power, so a battery that was discharging kept discharging into export when the house load dropped. A site that had either mode stored starts in manual self-consumption, saves that and logs it once; Home Assistant, the API and the app no longer accept either mode, and Settings no longer shows the per-battery weight field. Home Assistant mode changes now behave like the app's, so a mode that cannot be saved still reaches the planner and a replan no longer holds up other Home Assistant commands. The old `planner.use_energy_dispatch` key is converted to `planner.legacy_dispatch` on load and removed from stored settings, so a site that chose the legacy dispatch path keeps it.
