---
"forty-two-watts": minor
---

**Load model now adapts the heating coefficient online from measurements.**
Previously `HeatingW_per_degC` was operator-set and never moved — if the
value drifted from reality (or the house turned out not to track outdoor
temperature at all), forecasts silently inflated cold-day load and the
MPC made decisions on phantom heating draw.

The fit runs as one-parameter SGD on the prediction residual:
`coef ← coef + α · err / deltaT`. Gated on `bucket.Samples ≥
MinTrustSamples` (residual derives the slope from the bucket baseline)
and on `deltaT ≥ 3 °C` (warm samples and near-reference samples have no
heating signal to extract). Clamped to `[0, 1500] W/°C`.

The fit runs **before** the outlier filter so a wildly stale coefficient
can recover — without that ordering, every cold sample under a wrong
coef looks like an outlier vs the warm-day MAE and nothing could ever
pull the value down.

Operator config (`Planner.HeatingWPerDegC` / `SetHeatingCoef`) still
seeds the initial estimate and is re-applied on
`POST /api/loadmodel/reset`. From there observation drives the value.
Households whose load is temperature-independent (district heating,
solar-gain-dominated shoulder seasons, well-insulated homes) converge
toward 0 W/°C.

Found 2026-05-28 on site .40: planner predicted 2782 W load for a sunny
May afternoon (actual 504 W). Root cause was the un-adapted heating
term — `300 W/°C × (18 − 11.4 °C) = 1980 W` of phantom load applied
without seasonal / solar-gain awareness. The dispatcher fix in #375
prevents the *symptom*; this change addresses the *cause*.
