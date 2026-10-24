---
"ftw": patch
---

An Easee Cloud charger now starts a 6 A charge at once. Easee's firmware waits about five minutes before it starts a car at 6 A. The bundled `easee_cloud` driver (1.3.8) offers 7 A per phase for that start, and only when the per-phase cap that FTW sends with each order (`cap_amps_per_phase`) allows 7 A. It steps down to 6 A as soon as the charger reports charging, or after 90 s. A charger on a phase without room for 7 A starts at 6 A as before. `start_boost: false` in the driver config turns the 7 A start off.
