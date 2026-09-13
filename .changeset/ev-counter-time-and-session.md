---
"ftw": patch
---

Keep charger measurement times when estimating EV energy. Use fresh power between delayed session-counter updates, reconcile overlapping energy once, and retain the estimate through a verified session restart. Missing or older counters no longer reset a confirmed battery level. Expose the estimate source and measurement ages.

Match Easee pauses to the current vendor session even when sessionEnd is populated. Bound power estimates to its reporting cadence. Replan when a restored EV level differs from the active plan.
