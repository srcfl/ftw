---
"ftw": patch
---

Keep charger measurement times when estimating EV energy. Use fresh power between delayed session-counter updates, reconcile overlapping energy once, and retain the estimate through a verified session restart. Missing or older counters no longer reset a confirmed battery level. Expose the estimate source and measurement ages.
