---
"ftw": patch
---

Keep charger measurement times when estimating EV energy. Use fresh power between delayed session-counter updates, reconcile overlapping energy once, and retain the estimate through a verified session restart. Missing or older counters no longer reset a confirmed battery level. Expose the estimate source and measurement ages.

Match Easee pauses to the current vendor session even when sessionEnd is populated. Bound power estimates to its reporting cadence. Replan when a restored EV level differs from the active plan.

Show when a stopped charge has reached its target, and distinguish an estimated battery level from one reported by the car.

Pause dispatch when charger power is unavailable and retain spent pulse energy across recovery. Compare EV progress with the allowed duty curve. Bound progress checkpoints to 30 seconds or 30 Wh, with immediate saves for stops and user corrections.
