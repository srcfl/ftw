---
"ftw": patch
---

A driver that has just started is no longer marked offline before its first
reading. After every Core start, update or driver install, the watchdog sent the
driver its autonomous default a second time, 2–4 s after the start; an Easee
charger lost its charging current each time. A new driver now gets the watchdog
timeout from its start, and one that never reports still goes offline after it.
