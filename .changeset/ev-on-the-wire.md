---
"ftw": patch
---

EV charging is on the app wire. Field 10, `ev_w`, carries the summed charger draw — frozen in meaning like fields 1–9, conditional in presence like battery_soc: a site without a charger sends nothing and the app draws no EV node rather than a dead one. Charger drivers now also appear in the app's source table, so their freshness is visible. The energy-flow component gains a `static` attribute that holds every animation still — the FTW app sets it while showing a cached snapshot, because a moving particle is a claim that power is flowing right now.
