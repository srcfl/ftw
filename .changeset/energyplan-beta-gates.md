---
"ftw": patch
---

Keep the latest Core DP comparison queued when a replan cancels the previous
shadow. Cancellation no longer overwrites a comparison with a rejected verdict.
Reject active zero PV caps until dispatch can execute them. Keep the Python
PV-charge bonus aligned with Core in every mode. On hosts without a compiled
worker, verify bundle integrity and skip execution tests; keep Core as default.
Keep the release default when saving planner settings, and offer Energyplan
and Core DP as explicit choices.
