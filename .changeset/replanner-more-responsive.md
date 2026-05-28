---
"forty-two-watts": minor
---

feat(mpc): tighter replan triggers + twin-driven replan signal

Tightens the reactive replan thresholds (PV 500→250 Wh, load 400→200 Wh,
half-life 15→8 min, cooldown 60→30 s) and adds a third trigger that fires
when the PV or load twin's CURRENT prediction has shifted materially (RMSE
> 250 W PV / 200 W load over the next 16 slots) from the prediction the
active plan was built on.

The twin already self-corrects every cycle through RLS; the planner only
consumed its output every 15 min. The new signal closes that gap without
waiting for the integral-of-error to accumulate. Replanning is ~100 ms on
a Pi 4 (51 × 21 × 193 DP cells, sub-1 % CPU) — being stingy was the wrong
default.
