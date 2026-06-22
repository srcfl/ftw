---
"forty-two-watts": patch
---

A manual EV charge hold ("Start" / amp slider) now auto-releases when the
vehicle stops requesting current — e.g. it reaches its own charge limit
and is full. Previously the hold pinned the wallbox at a fixed amperage
and the loadpoint kept showing "charging" at 0 W until the operator
pressed Stop. The controller now drops the hold after the vehicle has
been not-requesting for SessionCompletionTimeout (90 s, debounced against
brief ramp/handshake dips), falling back to automatic dispatch. Only
applies to chargers that report "vehicle no longer requesting current"
(e.g. Easee); chargers that can't distinguish that are unaffected.
