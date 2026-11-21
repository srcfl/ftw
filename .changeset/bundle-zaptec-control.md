---
"ftw": patch
---

FTW now controls a Zaptec Go, Go 2 or Pro through Zaptec Cloud. The bundled
`zaptec_cloud` driver used to read the charger only, so FTW accepted it but
never steered it, and the car could overload a phase next to the battery.
It now sets the charging current within the fuse budget, pauses and resumes.
Not yet confirmed on a real charger. Tesla Wall Connector, TeslaMate, Home
Assistant cars and Tesla Cloud now say why FTW can only read them.
