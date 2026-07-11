---
"forty-two-watts": patch
---

fix(battery): honor configured soc_max at the driver, not just the planner

A battery's `soc_max` (and `soc_min`) only reached the planner, never the
driver. The Ferroamp driver therefore applied its own built-in
`CHARGE_CEIL_SOC` default of 0.95: once every ESO crossed ~95% it reported the
pack "not charge-capable" and idled the charge command, so a site configured
with `soc_max: 1.0` plateaued around 95–97% even while dispatch kept asking for
charge.

`config.WithBatterySoCBounds` now defaults each battery driver's
`charge_ceil_soc` / `discharge_floor_soc` from the matching
`batteries.<name>.soc_max` / `soc_min`, applied at both startup and config
hot-reload. An explicit value in the driver's own `config:` block still wins,
and the persisted config is never mutated (so a later `soc_max` change is not
shadowed by a stale derived value). `soc_max` is now the single source of truth
for a battery's usable SoC window.
