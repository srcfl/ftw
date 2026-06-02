---
"forty-two-watts": minor
---

**Replace the SoC safety floor with downside-PV planning.** The MPC's forecast-
risk reserve was a `soc_safety_floor_pct` (default 25 %) — a soft cost penalty
that kept SoC above a percentage on PV-surplus slots. A percentage is the wrong
unit (25 % of a 5 kWh battery and a 40 kWh battery hedge wildly different
absolute risk), it couldn't be set low or disabled (`0` was forced back to
25 %), and as a separate penalty it could fight legitimate "run down now, refill
cheap later" decisions.

The planner now instead optimises against **downside PV**: `PV_plan = forecast −
k·σ`, where σ is the live PV forecast-error std (the pvmodel residual std) and
`k = pv_forecast_safety_k` (default 1.0; `0` disables the hedge). The DP no
longer runs the battery down betting on PV that may not arrive, so a reserve
*emerges from the live forecast uncertainty itself* — large on variable cloudy
days, ~zero on clear days, and naturally inert in winter / no-sun (so passive
runs its charge-cheap / discharge-for-self-consumption loop down to the hardware
floor). No separate magic floor; the robustness comes from the economics.

**Config:** new `pv_forecast_safety_k` (pointer; unset → 1.0, explicit `0` →
no hedge). `soc_safety_floor_pct` and `safety_floor_penalty_ore_kwh_hour` are
deprecated — still parsed so existing config loads, but ignored with a warning.
Remove them and set `pv_forecast_safety_k` instead.
