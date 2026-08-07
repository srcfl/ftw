---
"ftw": minor
---

Core now actually sends the solar surplus to a driver whose Solar PV feed is armed. Every control tick computes the site's solar-attributable export — the smaller of live PV generation and grid export, after subtracting battery/V2X discharge so stored energy is never advertised as sunshine — and hands it, site-signed, to every driver whose operator enabled the write path (the `solar_pv` action, e.g. the NIBE S-series surplus feed). Dispatch runs behind the existing site-meter freshness gate: stale telemetry stops the feed and the driver's default mode / dead-man switch clears the device register. Standing refusals (pump-side enable still off) log once per transition instead of every tick.
