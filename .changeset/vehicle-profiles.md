---
"ftw": minor
---

Vehicle profiles for chargers shared by several cars. A new `vehicles:` config
list (also editable under Settings → Chargers → Vehicles) holds each car's
battery capacity, identifiers and charging policy — PV-surplus-only and/or a
target SoC the planner fills toward in the cheapest tariff hours. When an OCPP
charging session identifies the car (the RFID idTag on 1.6, a MacAddress or
eMAID idToken on 2.0.1), the charger switches to that car's capacity and
policy for the session; capacity reverts on plug-out. A session matching no
profile changes nothing — the visitor default — and the identity it presented
is shown in the Chargers tab so it can be pasted into a profile.
