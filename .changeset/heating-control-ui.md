---
"ftw": minor
---

The Heating view can now set a heat pump's curve offset. A pump whose driver
declares a control gets a row on its card: the value in force, when the hold
ends, and buttons to move it or release it. A pump that declares nothing looks
exactly as it did.

It lives on the Heating card rather than in Settings → Devices because that is
where the pump's own state already is — the offset sits next to the
temperatures it moves. Settings is for connecting a device, not for running it.

Rendered entirely from the declaration: label, bounds, step and unit come from
the driver, and nothing in the view knows a driver by name. Stepper buttons
rather than a slider or a number field, because the card is re-rendered every
30 s and a control holding input state would lose a half-typed value on each
refresh.

With no hold the row reads "Auto" rather than a number. Nothing in the browser
knows what offset the pump has settled on internally, and printing 0 would
claim knowledge we do not have. Held state is carried by the text and the
weight of the value, never by colour: the theme's green/red pair is not
separable under deuteranopia.

A driver that declares `evidence: "write_ack"` instead of `"readback"` says so
in the row — "the pump does not confirm this setting". The weaker guarantee
belongs where the operator is standing.
