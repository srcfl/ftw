---
"forty-two-watts": minor
---

**Settings UI: expose `pv_forecast_safety_k` on the Planner tab.** The
downside-PV safety factor (v0.111.0) was config-only; it now has a "PV forecast
safety (k)" field under Settings → Planner (default 1.0, with inline help).
Operators can dial it down to 0 to use the full battery, or up to keep more
reserve on uncertain days, without editing config.yaml.
