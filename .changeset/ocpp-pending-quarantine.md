---
"ftw": minor
---

OCPP charge points now start quarantined. A charge point that no charger entry
(loadpoint) names connects as "pending": it shows on Settings → Chargers with
its vendor, dialect and live state so it can be adopted, but its telemetry is
withheld from the site — no DerEV reading, no driver health, no metrics — and
it is never commanded. This stops any device that merely knows the shared OCPP
password from fabricating EV load and steering dispatch (the DerEV sum
suppresses home-battery discharge). Adopting a charger = adding a charger
entry with its id as the charger driver and saving — charger entries
hot-reload, so adoption and un-adoption take effect on the save.
