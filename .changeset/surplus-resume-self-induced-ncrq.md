---
"forty-two-watts": patch
---

fix(loadpoint): resume PV surplus after a self-induced charger stop (NCRQ)

When a surplus_only loadpoint was paused below its 3-phase floor on a sub-floor
PV dip, the charger reported the vehicle "not requesting current" (NCRQ). That
stop is self-induced — we withheld power, the car didn't decline — but it was
counted toward session completion. After the 90 s timeout the loadpoint latched
the session "complete" and the planner stopped offering PV surplus for the rest
of the day, so the charger never restarted when the sun returned (the home
battery soaked the surplus instead).

Two complementary fixes, both generic across EV-charger drivers:

- The controller now tells the loadpoint manager when it is withholding power
  (`SetSurplusWithheld`). A "not requesting current" report during a
  self-induced pause no longer advances the session-completion timer; a genuine
  vehicle-side refusal once power is offered again still completes as before.
- Chargers with no vehicle-API binding (e.g. a bare CTEK) couldn't be woken
  from NCRQ because the wake path required a bound vehicle driver. The
  controller now drives the wake off the charger's own connector state: when
  surplus recovers and we offer power but the charger is still in a
  self-induced NCRQ, it cycles the contactor (ev_pause → ev_resume) to make the
  vehicle renegotiate, throttled to once per cooldown.
