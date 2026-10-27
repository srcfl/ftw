---
"ftw": patch
---

An OCPP charger that a loadpoint names now starts at 0 A when it boots. Until now it ran at its own default, usually its full rating, until FTW's first order landed, so a car plugged in during that gap could draw 16 A per phase on a fuse FTW had not yet shared out. FTW's first order replaces the 0 A limit. A charger that refuses it is logged and not asked again until its next boot. A charger listed only under `ocpp.chargers`, which FTW meters but never orders, is left alone.
