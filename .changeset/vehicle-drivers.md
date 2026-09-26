---
"ftw": minor
---

FTW now ships three read-only vehicle drivers from srcfl/device-drivers. `teslamate_vehicle` reads a Tesla's charge state from the owner's TeslaMate MQTT broker, `tesla_cloud` reads it from Tesla's Fleet API, and `vag_vehicle` reads VW, Audi, Škoda, SEAT and Cupra charge state from the VW Group EU Data Act portal. None of them wakes a car or starts a charge. All three are experimental and have not yet run against a car. Add one in the config YAML; each driver's header comment shows the fields it needs.
