---
"forty-two-watts": patch
---

fix(pvmodel): MPC now consumes the unanchored structural PV predictor so the rolling residual correction (PR #381) is not applied twice. Previously `mpcSvc.PV` was wired to `pvSvc.Predict`, which already folds in the live-vs-model now-anchor; combined with `PVResidualCorrect` the planner saw the structural-vs-live bias subtracted twice and could plan as if PV was ~0 W on a sunny day with a heavy downward residual. A new `pvmodel.Service.PredictStructural` returns the RLS-only prediction; the anchored `Predict` is kept for UI overlays and dispatch's live-reading path.
