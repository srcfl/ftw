---
"ftw": patch
---

Say when FTW reads a charger, battery, car or heat pump but cannot control it.

A driver that declares `read_only = true` used to show only a small
"telemetry only" badge in Settings, so an owner who added a Zaptec Go through
`zaptec_cloud` saw FTW accept the charger and never steer it, with nothing to
say why. Now the EV card, Settings → Devices, Settings → Chargers, the device
cards when adding one, the setup wizard and the advanced charger panel say
"FTW reads this charger but cannot control it", with the driver's own reason
when its description gives one. `/api/drivers/catalog` entries carry
`read_only_reason`, and `/api/loadpoints` carries `driver_read_only` and
`driver_read_only_reason`. Meters and solar inverters, where control is not
expected, get no note. No setting changes.
