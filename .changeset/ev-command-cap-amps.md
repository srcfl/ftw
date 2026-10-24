---
"ftw": patch
---

Each charging order now tells the charger's driver how much current per phase FTW allows that charger right now, in a new `cap_amps_per_phase` field. It is the charger's share of the room below the fuse and the import limit, after the split between chargers, within the order's `max_amps_per_phase` and the charger's maximum. Until now a driver saw only the fuse rating. The Easee Cloud driver needs this to start a 6 A charge at 7 A only when 7 A fits, so a car on a tight phase is not stopped and restarted. The field adds no new limit, and FTW sends the same orders as before.
