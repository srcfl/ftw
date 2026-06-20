---
"forty-two-watts": patch
---

Driver cards no longer show phantom battery-SoC / PV / meter "0" rows for
telemetry-only drivers. A driver that emits only scalar metrics (e.g. the
MyUplink heat pump) has no meter/pv/battery DER reading, but the driver
card fell through to the meter/pv/battery layout and rendered 0 W / 0 %
with an empty SoC bar. Such drivers now render a compact "telemetry only"
body (status + ticks + errors); open Diagnose / the Heat pump card for
their signals.
