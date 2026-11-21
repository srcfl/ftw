---
"ftw": patch
---

Say when FTW reads a charger, car, heat pump or battery but cannot control it.

A driver that declares `read_only = true` used to show only a small
"telemetry only" badge in Settings, so an owner who added a Zaptec Go through
the read-only `zaptec_cloud` saw FTW accept the charger and never steer it,
with nothing to say why. Now the EV card, Settings → Devices, Settings →
Chargers, the device cards when adding one, the setup wizard and the advanced
charger panel say "FTW reads this charger but cannot control it", with the
driver's own reason when its description gives one. The EV card leaves out
Charge now and Pause charging for such a charger; a hold set before still
shows so it can be returned to the plan. A charger set to Observe only reads
the same way.

`/api/drivers/catalog` entries carry `read_only_reason`, and `/api/loadpoints`
carries `driver_read_only` and `driver_read_only_reason`, taken from the
driver that runs. Meters, solar inverters and gateways that also read a
battery, such as Zap, get no note. No setting changes.
