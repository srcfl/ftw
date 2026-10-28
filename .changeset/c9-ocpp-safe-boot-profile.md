---
"ftw": patch
---

An OCPP charger that a loadpoint names now starts at 0 A when it boots and reports no car. Until now it ran at its own default, usually its full rating, until FTW's first order landed, so a car plugged in during that gap could draw 16 A per phase on a fuse FTW had not yet shared out. FTW waits until the connector statuses after the boot have been quiet for one second, and any socket that reports a car cancels the limit, so a charger that reconnects to a restarted Core with a car charging on any socket is not paused. FTW's first order replaces the 0 A limit. A charger that refuses it is logged and not asked again until its next boot. A charger listed only under `ocpp.chargers`, which FTW meters but never orders, is left alone.
