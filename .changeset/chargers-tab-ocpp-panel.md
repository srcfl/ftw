---
"ftw": minor
---

Rename the Loadpoints tab and dashboard section to Chargers, and give it an
OCPP panel: the exact backend URL to enter on a charger, live state for every
connected charge point (vendor, dialect, vehicle, power, session energy), and
the connected charge points offered in the charger-driver dropdown so an OCPP
charger can be bound to the planner without editing YAML. Backed by a new
GET /api/ocpp/chargers endpoint. Docs now recommend a DHCP reservation for the
FTW host before commissioning chargers.
