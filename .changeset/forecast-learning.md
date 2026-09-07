---
"ftw": minor
---

Correct weather interval timing, panel direction and forecast energy. Train household and PV models only on fresh, complete measurements. Keep grid limits separate from household demand and let PV learn its scale without a battery-based guess.

Run Energyplan 0.2.0 PV and load models locally as forecast candidates, with no required panel geometry. Save issued forecasts and model state for causal comparison, report uncertainty by horizon, and retain the current planner forecast while gathering evidence. Add a read-only forecast evaluation command. No candidate promotion or cloud connection is required.
