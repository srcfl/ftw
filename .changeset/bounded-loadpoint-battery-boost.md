---
"ftw": minor
---

Add a per-loadpoint Battery boost lease with a four-hour maximum, explicit home-battery reserve, optional EV target and departure, restart-safe persistence, API controls, and visible stop reasons in the Loadpoints UI.

Battery boost automatically stops on expiry, reserve, unplug, operator holds, surplus-only policy, stale or unavailable meter/drivers, incompatible core modes, and fuse safety. The legacy site-wide `battery_covers_ev` control remains available for compatibility.
