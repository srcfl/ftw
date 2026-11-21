---
"ftw": minor
---

FTW now controls a Zaptec Go, Go 2 or Pro through Zaptec Cloud. The bundled
`zaptec_cloud` driver used to be read-only, so a Zaptec charger on a
loadpoint charged at whatever the Zaptec app set, and the car could overload
a phase next to the battery. FTW now sets the charging current within the
fuse budget, pauses and resumes. When FTW stands down, for example on a
restart or a stale meter, it sets the charger to 0 A and pauses it until the
next order; the setting is stored on the charger, so if you stop using FTW,
set the current again in the Zaptec app. Experimental: not yet confirmed on a
real charger. Tesla Wall Connector, TeslaMate, Home Assistant cars and Tesla
Cloud now say why FTW can only read them.
