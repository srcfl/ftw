# loadmodel — household load predictor (hour-of-week + heating)

## What it does

168 hour-of-week EMA buckets plus an operator-configured heating
coefficient. Captures the weekly pattern that dominates residential load
(morning peak, evening peak, weekend shift) without any basis functions.
Trust-weighted blend with a baked Swedish-home prior makes day-one
predictions plausible while the model refines itself from telemetry.

## Math

Bucket index (`model.go:102-106`):

```
HourOfWeek(t) = ((weekday + 6) % 7) * 24 + hour   (Mon=0 .. Sun=6)
```

Per-bucket blend (`model.go:111-131`):

```
trust   = min(bucket.Samples / 8, 1)    (MinTrustSamples = 8)
base    = trust*bucket.Mean + (1-trust)*typicalPrior(idx)
heating = max(HeatingReferenceC - tempC, 0) * HeatingW_per_degC
y       = base + heating                 (clamped to [0, 3*PeakW])
```

Bucket update (`model.go:161-175`) — exact running mean for first 10
samples, then EMA with `alpha = 0.1`. Subtracts the current heating estimate
so the bucket tracks "base" load only:

```
baseSample = max(actualLoadW - heatEst, 0)
bucket.Mean += alpha * (baseSample - bucket.Mean)    (after 10 samples)
```

Outlier gate: after 50 global samples, reject `|err| > max(10*MAE, 200 W)`
(`model.go:151-156`).

Baked prior (`model.go:67-81`): overnight 300 W, morning peak ~2000 W at
07:00, midday 600 W at 13:00, evening peak ~2500 W at 18:30 (19:00 on
weekends, with softened morning). `HeatingReferenceC = 18 °C`.

## Inputs / outputs

Per sample: `(t, actualLoadW, tempC)` where actualLoadW is derived in
`Service.sample` from online telemetry only:

```
loadW = grid_w - pv_w - bat_w - ev_w     (site sign, negative skipped)
```

`Predict(t) float64` returns expected W. Uses `TempFunc` if wired, else
assumes `HeatingReferenceC` (no heating contribution).

## Training cadence + persistence

Sample every `SampleInterval = 60s` (`service.go:44`). Skips when the site
meter reading isn't available or `loadW < 0` (driver transient,
`service.go:131-150`).

Persistence: `state.Store.SaveConfig("loadmodel/state_utc", …)` — constant at
`service.go:19`. Persists every `PersistEvery = 10` samples and on stop.

Reset: `POST /api/loadmodel/reset` preserves the configured heating
coefficient.

`HeatingW_per_degC` is operator-set via `Service.SetHeatingCoef` (called
from config reload). Not fit online — too noisy and entangled with bucket
baseline (`model.go:179-184`).

## Public API surface

Model (`model.go`):

- `NewModel(peakW) *Model`, `HourOfWeek(t) int`
- `(Model).Predict(t, tempC) float64`, `.PredictNoTemp(t) float64`
- `(*Model).Update(t, actualLoadW, tempC) bool`
- `(Model).Quality() float64`
- Constants `Buckets = 168`, `MinTrustSamples = 8`, `HeatingReferenceC = 18`.

Service (`service.go`):

- `NewService(st, tel, siteMeter, peakW) *Service`
- `(*Service).Start(ctx)` / `.Stop()` / `.Reset()`
- `(*Service).Predict(t) float64`, `.Model() Model`
- `(*Service).SetHeatingCoef(w float64)`
- Injected func type `TempFunc` (forecast integration).

## How it talks to neighbors

- MPC consumes `Service.Predict` via the `mpc.LoadPredictor` func type
  (`mpc/service.go:22`, wired in `main.go`).
- Site meter name passed in from config; reads via
  `telemetry.Store.Get(siteMeter, DerMeter)` + aggregates PV + battery.
- `TempFunc` injected from the forecast cache so the heating term is
  forecast-aware when planning cold nights.

## What NOT to do

- Do not use this to learn heating sensitivity — leave
  `HeatingW_per_degC` as an operator input. Online fit co-varies with the
  bucket baseline and gives noise.
- Never train on `loadW < 0`; transient flows during PI steps can appear
  negative. `sample()` already skips, keep it that way (`service.go:145`).
- Do not remove the "subtract heating before EMA" step — a bucket should
  learn base load, not "base + this week's weather".
- Do not change `Buckets` without migrating stored state.
