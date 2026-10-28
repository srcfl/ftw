---
"ftw": patch
---

A battery at or under its minimum level no longer discharges. Before, a battery at exactly 5.0 % (or exactly at `soc_min`) still counted as above the floor. Planner modes now keep `soc_min` with a fresh plan too, not only when the plan is missing. A discharge already running stops at the floor even when the grid sits inside the deadband; before, the battery kept its last setpoint and drained on. Plain `self_consumption` and manual holds still use Core's 5 % floor.
