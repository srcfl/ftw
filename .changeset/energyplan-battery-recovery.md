---
"ftw": patch
---

Update the compiled Energyplan worker to 0.1.2. It now plans from the real
battery SoC below the reserve or above the charge limit, while keeping each
step within Core's recovery rules. Low SoC no longer forces Core DP fallback;
Core DP stays as the comparison shadow and reserve planner.
