---
"ftw": patch
---

Command the battery on every control tick at the default settings. The dispatch holdoff no longer skips about every other tick: it is off when `min_dispatch_interval_s` is not longer than `control_interval_s`, and otherwise allows 0.25 s of tick jitter.
