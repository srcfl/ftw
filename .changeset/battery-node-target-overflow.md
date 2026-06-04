---
"forty-two-watts": patch
---

ui(flow): drop the "· charging/discharging" suffix on battery nodes

The battery node in the energy-flow view showed `target −83 W · discharging`,
which overflowed the node circle. The suffix is now removed — the live W value
and SoC% already convey direction — so the label reads just `target −83 W`.
