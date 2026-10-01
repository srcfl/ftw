---
"ftw": patch
---

Forecast error bands and baselines now carry over across Core updates that leave forecasting alone. Before, each update started them over, so the planner's forecast margin always used its widest cold-start bands.
