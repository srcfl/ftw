---
"forty-two-watts": patch
---

Set the car's SoC from the EV Charger modal. Clicking the EV planet now
shows a "State of charge" section: while the car is plugged in it offers
an inline % field + Set (re-anchors the inferred SoC via
POST /api/loadpoints/{id}/soc and replans); unplugged it shows the current
value with a hint. Previously this lived only in the advanced loadpoints
panel — now it's where the SoC is naturally looked for.
