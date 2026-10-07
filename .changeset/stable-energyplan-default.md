---
"ftw": patch
---

Use Energyplan by default on stable releases too, as betas already do. A beta box that moves to stable keeps the planner it has run, and a new stable install starts with Energyplan. Core DP still runs as shadow and fallback, and `planner.engine: core` still selects it.
