---
"forty-two-watts": patch
---

Settings → Planner: the Mode dropdown is gone — it edited a config field that the runtime strategy (set from the dashboard Plan card) overrides, so it showed stale values and confused operators. In its place a read-only "Active strategy" row shows what the planner is actually running. The "PV forecast safety (k)" help text now explains the real mechanism (plans against forecast − k·σ; higher k holds more reserve and charges earlier, it never forces charging), with a live σ/hedge readout under the field.
