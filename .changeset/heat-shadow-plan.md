---
"ftw": minor
---

Heat zones can now be planned. A new `heat` section names each zone's
sensor, control, boundary, heat output, target and hard band, and the owner's
own rules: price comfort, peak relief and a least saving per day for all
zones together. No number has a default. Core plans every quarter from the
thermal twins, the stored prices and the weather forecast, and shows the plan
and what it would save at `GET /api/heat/plan`. Nothing is written unless
`heat.write` is on; then a zone's setpoint is held through the same control
holds as an operator's request, only while its twin is trusted, the least
saving is reached and the site meter is fresh, and the device is handed back
to its own default otherwise. A device that drops a written value by itself is
written again before it does.

A zone can name its heat input (`heat_input: "<driver>/<metric>"`), so a room
sensor on one device, such as an underfloor gateway, learns against the heat
of another, such as a heat pump's estimated space heat. A sensor named in
`heat.zones` is learned even when its driver does not declare it.

A zone can be heated by a heat pump (`source: heat_pump`) with the owner's
efficiency and heat curve, and planned through a declared curve offset with
the owner's figures for what one unit does.

Every plan is logged to `heat-shadow.jsonl` in the data directory, one line
per zone and quarter: what the plan and the device left alone would do in the
quarter starting, and how the quarter that ended came out against what was
said for it, with the zone's temperature at both ends, the heat input's
energy over the quarter and how much of it was covered by fresh readings. In
shadow the device runs itself, so the left-alone prediction is what the
readings can confirm before `heat.write` is trusted. Each quarter also gets
the temperature the planner's own model gives for the heat that actually went
in, which tests that model whoever controls the zone, and the electricity it
took and what that cost at the quarter's price. For a heat pump that
electricity is the whole pump's draw, hot water included. The file is moved
aside at 32 MB.
