---
"ftw": patch
---

One slow driver can no longer stall dispatch for the whole site. Every command the control tick sends now carries its own deadline, derived from `site.control_interval_s` (half the interval, capped at 2 s and floored at 250 ms). Before this, the tick handed the driver registry the process-lifetime context, which has no deadline, and the driver goroutine runs the device call inline — so a cloud driver waiting on an HTTP or OAuth request that never answered held up every other battery on the site, and the reactive fuse guard with them. A command that runs out of time is logged at Warn with the driver name, so a chronically slow driver shows up in the log instead of quietly eating tick cadence.
