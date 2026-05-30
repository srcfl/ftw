---
"forty-two-watts": patch
---

**Easee driver: pause+resume the contactor on a live phase flip so 1Φ→3Φ
actually takes effect.** The Easee only latches its phase count when a session
(re)starts — writing `phaseMode=3` while a session is actively charging at 1Φ
leaves the contactor on a single phase, so a loadpoint that crossed from 1Φ to
3Φ (e.g. a schedule ramping to 11 kW) stayed throttled to ~3.7 kW. Field-
confirmed: only a manual pause+resume flipped it.

The driver now pauses charging before writing the new `phaseMode` on a real
mid-session flip (`last_sent_phases` already set); the existing auto-resume
(offer > 0 while paused) re-closes the contactor on the new phase count. The
first command of a session is unaffected (no live contactor to recycle).
