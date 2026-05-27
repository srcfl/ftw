---
"forty-two-watts": minor
---

Expose `CHARGE_CEIL_SOC` and `DISCHARGE_FLOOR_SOC` in the Ferroamp Lua
driver as operator-tunable YAML config fields.

```yaml
- name: ferroamp
  config:
    charge_ceil_soc: 1.0       # default 0.95 — charge all the way to 100%
    discharge_floor_soc: 0.05  # default 0.15 — discharge down to 5%
```

Both fields are optional and default to the existing constants, so
existing configurations behave identically. Out-of-range or
non-numeric values are logged as warnings and the default is kept. To
actually reach 100 % SoC the operator must also raise
`planner.soc_max_pct` — the planner cap and the driver cap are two
independent layers.
