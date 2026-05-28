---
"forty-two-watts": minor
---

**PV twin now applies a short-horizon residual correction on top of the
structural RLS prediction.** The RLS model's forgetting factor (~3h
half-life @ 60s cadence) is tuned to learn site orientation, shading
and slow soiling drift; it does not respond fast enough to "today's
persistent NWP bias" — e.g. when measured cloud cover is heavier than
the forecast assumed for the last 90 minutes, structural predictions
stay biased high while RLS chews through the samples needed to adapt.

The new layer keeps a 2-hour rolling buffer of (predicted_at_t,
actual_at_t) pairs, computes the mean residual, and applies it as an
additive bias to MPC slot predictions, fading linearly over a 2 h
horizon (full correction ≤ 30 min, zero by 120 min). Beyond 2 h the
structural model is again the best estimate — weather fronts roll in,
time-of-day shifts, and the residual is no longer relevant.

Gates (`go/internal/pvmodel/residual.go`):
- ≥ 20 samples in the 2 h window before any correction applies.
- `|mean residual|` ≥ 25 W → otherwise treated as "no bias detected".
- `std / |mean|` ≤ 1.0 → variance-dominated streams are skipped.
- `dt ≤ 0` (past slot) → factor = 0.

Wiring: `pvmodel.Service.ResidualCorrect` is plumbed into
`mpc.Service.PVResidualCorrect` (new optional hook). The planner calls
the corrector on the slot midpoint inside `buildSlots`, after the twin
prediction and before `selectPlannerPVW` blends with the NWP forecast.
A nil hook is a hard no-op, so existing wiring without the corrector
is unchanged.

**PV only**: load is multimodal (appliances cycling) and a rolling-mean
correction can chase the noise. Variance gate would catch it most of
the time, but risk/reward is poor without dedicated diagnostics.
Revisit when load observability lands.

Diagnostics exposed via `GET /api/pvmodel`:
`pv_residual_correction_w` (the value the planner would apply 15 min
out), `pv_residual_sample_count`, `pv_residual_mean_w`,
`pv_residual_std_w`, `pv_residual_window_minutes`.
