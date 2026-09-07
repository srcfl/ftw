---
"ftw": minor
---

Correct weather interval timing, panel direction and forecast energy. Train household and PV models only on fresh, complete measurements. Keep grid limits separate from household demand and let PV learn its scale without a battery-based guess.

Use the local Energyplan PV and load models as the planner's first forecast source, with no required panel geometry. Keep the previous forecast as a shadow and use it when the new model lacks a valid prediction or its worker fails. Save issued forecasts, per-signal sources and model state for causal comparison, report uncertainty by horizon, and add a read-only forecast evaluation command.
