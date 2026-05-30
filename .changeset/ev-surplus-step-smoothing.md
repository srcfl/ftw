---
"forty-two-watts": patch
---

**surplus_only EV charging: smooth the step setpoint so the EV and home
battery stop fighting over the same PV surplus.** The surplus_only setpoint
magnitude tracked the *instant* surplus and snapped to an `allowed_steps_w`
step every 5 s tick. Because `surplusW = −gridW + batW + evW` counts the home
battery's current charge power as EV-available, a single-tick wobble (the
battery briefly backing off, a cloud edge, a load twitch) ratcheted the EV up
a step it couldn't hold — it collapsed the next tick, and the repeated
multi-kW load swing whipsawed the home battery's reactive PI into integrator
windup, so the battery stopped delivering its planned discharge (an EV↔battery
limit cycle; observed live as `ev_w` swinging 0–4.7 kW and the battery
under-delivering to ~4% of plan).

The step setpoint now uses **asymmetric smoothing**: down-steps still track the
instant surplus (the no-import promise is unchanged), but an **up-step is gated
on the rolling average** — the EV only climbs to a higher step when the smoothed
surplus sustains it. This breaks the limit cycle: the EV ramps up only on a
genuine surplus rise and the home battery's PI stays stable. Pause/resume
hysteresis and the no-import guarantee are untouched.
