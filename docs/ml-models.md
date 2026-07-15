# Learning models

Five self-learning components sit between raw telemetry and the MPC
planner. Four are digital twins of physical parts of the site (PV, load,
price, battery); the fifth is the planner itself, which searches over
their outputs to pick a schedule.

All twins share the same three invariants:

1. Cold-start with a hand-picked prior — day-0 predictions are usable.
2. Blend learned output with the prior by a trust ratio — one bad RLS
   step cannot produce a wild forecast.
3. JSON-persist state through `state.Store.SaveConfig(...)` /
   `state.Store.SaveBatteryModel(...)`; restore on boot.

Site sign convention holds throughout: positive W = into site, so PV is
stored negative and twins emit positive W that the MPC flips at the API
boundary (`buildSlots` in `go/internal/mpc/service.go:465`).

Forward references to sibling chapters landing in parallel:
`docs/mpc-planner.md`, `docs/battery-models.md`, `docs/api.md`,
`docs/safety.md`.

---

## 1. PV digital twin (`go/internal/pvmodel`)

### Model

Linear recursive-least-squares over a 7-feature vector, one RLS step per
60 s sample. Built at `go/internal/pvmodel/model.go:75`:

```go
func Features(clearSkyW, cloudPct float64, t time.Time) [NFeat]float64 {
    cf := math.Pow(1-cloudPct/100.0, 1.5)
    h  := 2 * math.Pi * (hour_of_day) / 24
    return [7]float64{
        1,
        clearSkyW,
        clearSkyW * cf,
        clearSkyW * math.Sin(h),  clearSkyW * math.Cos(h),
        clearSkyW * math.Sin(2*h), clearSkyW * math.Cos(2*h),
    }
}
```

The first two features carry bias + clear-sky level; feature 3 is the
naive `rated × clearsky × cloud_factor` physics prior; features 4..7 are
first + second hour-of-day harmonics that absorb orientation, tilt, and
morning/afternoon shading. Forgetting factor 0.995 (~200-sample
window).

### Inputs and outputs

- **Inputs**: clear-sky irradiance W/m² (via `ClearSkyFunc` injected from
  `go/internal/sunpos` or the `forecast` package), cloud cover % (from
  the weather forecast cache), time, observed PV W (summed positive
  across `telemetry.DerPV` readings).
- **Outputs**: `Predict(clearSkyW, cloudPct, t)` returns non-negative
  predicted PV W at any future time. The MPC's `buildSlots` negates it
  to site sign before handing it to the DP.

### Training

Service loop in `go/internal/pvmodel/service.go:150`:

- 60 s sample interval (`SampleInterval`).
- Skip when `clearSkyW < 50` W/m² (night; no signal).
- Skip when all PV drivers report ~0 while clear-sky is meaningful
  (driver outage guard).
- Outlier guard: if `actualPVW > 1.2 × ratedW` the sample is rejected
  — sensor-noise envelope (`model.go:158`).
- Cold-start poisoning guard: if the model's own prediction `ŷ > 2 ×
  ratedW` before the update, skip — keeps one bad sample from
  cascading into wild β (`model.go:170`).
- After the first 50 samples, residuals outside `max(10 × MAE, 200 W)`
  are rejected (`model.go:175`).

### Output sanity envelope

`Predict` clips to [0, 1.05 × rated]. If the learned β produces
`y > 1.05 × ratedW` the function falls back to the physics prior
instead, which is bounded by construction (`model.go:140`).

### Cold-start blending

```go
trust := float64(m.Samples) / WarmupSamples   // WarmupSamples = 50
if trust > 1 { trust = 1 }
y := trust*learned + (1-trust)*prior
```

`WarmupSamples = 50` (`model.go:103`). Day one, `trust = 0` so PV
prediction is 100 % physics prior — identical to the pre-twin behaviour.
After ~50 usable daylight samples (roughly a sunny day) it is fully the
learned model.

### Persistence

JSON under `state.Store.LoadConfig("pvmodel/state")` /
`SaveConfig("pvmodel/state", …)`. Key defined at
`go/internal/pvmodel/service.go:15`. Restored on boot with the
configured `ratedW` re-applied so a config change (new panels)
immediately takes effect on the physics prior without losing β.

### Reset

`POST /api/pvmodel/reset` (`go/internal/api/api.go:120`). Re-seeds via
`NewModel(ratedW)`: β zeroed except β[2] = ratedW/1000 (the naive
clear-sky coefficient), covariance diag = 1000, sample count = 0.

### Future work: `sunpos`-based auto-PV

`go/internal/sunpos` is a physics-only solar-position + plane-of-array
irradiance library (Spencer 1971 series). It already feeds `ClearSkyFunc`
for the twin's prior. The planned step is per-array tilt+azimuth fitted
from clear-sky days, so a multi-array site with mixed orientation gets a
per-array physics prior that the RLS residual then tunes for shading.
Tracked separately; not in the current twin.

---

## 2. Load digital twin (`go/internal/loadmodel`)

### Model

168 hour-of-week buckets (7 days × 24 hours). Each bucket holds an EMA
of observed load. Defined at `go/internal/loadmodel/model.go`:

```go
const Buckets         = 7 * 24   // 168
const MinTrustSamples = 8
const HeatingReferenceC = 18.0   // °C
```

Trust-weighted blend with the baked prior at prediction time:

```go
trust := bucket.Samples / MinTrustSamples    // capped at 1
base  := trust*bucket.Mean + (1-trust)*profilePrior(idx)
heat  := HeatingW_per_degC * max(18 - tempC, 0)
return base + heat
```

The first 10 samples of a bucket use an exact running mean (crisp
convergence); after that the model switches to EMA with α = 0.1 (smooth
drift as the home evolves). `model.go:171`.

### Heating coefficient

`HeatingW_per_degC` is operator-declared via
`Planner.HeatingWPerDegC` in config. Online fit was tried and rejected
(noisy + entangled with the bucket baseline). The bucket update
subtracts the current heating estimate so buckets learn "base" load
only (`model.go:167`).

### Inputs and outputs

- **Inputs**: derived measured house load
  `grid_w − pv_w − bat_w − ev_w` (site sign) pulled from online
  `telemetry.Store` readings once per 60 s; outdoor temperature from the
  forecast (`TempFunc`). EV is subtracted so car charging does not train
  the household weekly pattern. Negative loads are treated as PI-step
  transients and skipped (`service.go:144`).
- **Outputs**: `Predict(t)` returns expected load W at any future
  timestamp. MPC `buildSlots` passes it directly as `LoadW` (already
  site-positive).

### Cold-start

Baked single-family Swedish-home prior at `model.go:67`:

- 300 W overnight baseload (Gaussian peaks on top).
- ~2000 W morning peak at 07:00 (weekday), ~30 % lower on weekends.
- ~600 W midday lobe at 13:00.
- ~2500 W evening peak at 18:30 weekday / 19:00 weekend.

Every bucket starts at that prior, so day-0 MPC gets a plausible load
curve.

### Occupancy profiles

The service maintains two independent profiles:

- `home` — default profile, seeded with the normal household prior.
- `away` — manual away profile, seeded with a lower unoccupied-house
  prior and trained only while selected.

The active profile controls both online training and MPC predictions.
Switching profile through `POST /api/loadmodel/profile` persists the
selection and triggers an immediate MPC replan so the next dispatch cycle
uses the matching load forecast.

### Persistence + reset

- Config keys: `loadmodel/profile` and
  `loadmodel/state_utc:<profile>` (`service.go`).
- Legacy `loadmodel/state_utc` migrates into the `home` profile when no
  profile-specific home model has been stored yet.
- `POST /api/loadmodel/reset` (`go/internal/api/api_loadmodel.go`) — reseeds
  the active profile while preserving the configured peak and heating
  coefficient.

---

## 3. Price forecaster (`go/internal/priceforecast`)

### Model

Per-zone hour-of-week × month profile:

- 168 buckets of öre/kWh (raw spot) per zone.
- 12-entry monthly multiplier (seasonal ratio vs annual mean).
- Bayesian blend with the baked prior on every refit:

```go
// go/internal/priceforecast/forecast.go:197
bucket[i] = (prior[i]*PriorWeight + sum_of_observed[i]) /
            (PriorWeight + count_of_observed[i])
```

`PriorWeight = 8` (`forecast.go:163`). Two real samples give 80 % prior
+ 20 % data; at ~40 samples the prior is down to ~17 %. This is the
bug-fix the prompt flagged: a pure EMA collapsed the shape when the
first few history rows landed in one bucket.

### Inputs and outputs

- **Inputs**: the last 90 days of `state.PricePoint` rows from
  `state.Store.LoadPrices(zone, since, until)` (`forecast.go:361`).
- **Outputs**: `Predict(zone, t) → öre/kWh` spot. Used by MPC to
  back-fill slots past the day-ahead publication window —
  `extendPricesWithForecast` in `go/internal/mpc/service.go:410`
  synthesizes `state.PricePoint` rows tagged `source="forecast"` for
  the >24 h tail of the 48 h horizon.

### Cold-start prior

`bakedPrior(zone)` in `forecast.go:79`:

- Morning ramp 07:00–09:00 (1.6 × base).
- Evening peak 17:00–20:00 (1.85 × base).
- Midday trough 11:00–14:00 (0.55 × base).
- Overnight baseline 00:00–05:00 (0.65 × base).
- Weekend peaks dampened ~15 %.
- Base is per-zone: SE3/SE4 = 80, NO2/FI = 70, SE1/SE2/NO1/NO3/NO4 = 50,
  default 60 öre.
- Month multiplier peaks in Jan/Dec (1.35/1.40) and bottoms in July
  (0.70).

`NewZoneModel` seeds `Counts[i] = MinTrustSamples` so `Predict` on a
cold zone returns the shaped prior — no "flat mean" zone ever.

### Confidence

ML-forecasted slots are tagged `source="forecast"` in the store;
`buildSlots` sets `Slot.Confidence = 0.6` for those rows
(`go/internal/mpc/service.go:493`). The DP blends low-confidence prices
toward the horizon mean:

```go
// go/internal/mpc/mpc.go:195
effPrice(s) = s.Confidence*s.PriceOre + (1-s.Confidence)*meanPrice
```

So at c = 0.6 a predicted 200 öre peak is weighted as ~160 öre when the
DP compares actions. Reported `cost_ore` on actions uses raw prices —
blending is a decision lens, not an accounting change
(`mpc.go:350`-`366`).

### Persistence + refit

- Config key: `pricefc/state` (`forecast.go:243`).
- Refit interval: every `RefitInterval = 6 h` (`forecast.go:246`).
- No reset endpoint — sparse history + the Bayesian prior keep the
  model stable; a reset would just wipe learned months and rebuild from
  scratch next tick.

### CSV seed

`SeedFromCSV` (`forecast.go:414`) ingests `zone,slot_ts_ms,slot_len_min,spot_ore_kwh`
rows, UPSERTs into `state` (idempotent on `(zone, slot_ts_ms)`), and
kicks a refit. Years of history makes unusual calendar days (bank
holidays that stay cheap on weekdays) forecastable.

---

## 4. Battery dynamics twin (`go/internal/battery`)

### Model

ARX(1) discrete-time state equation (`go/internal/battery/model.go:212`):

```
y(t+1) = A · y(t) + B · u(t) + noise
```

Where `u` is the commanded site-sign power (W, + = charge) and `y` is
the smoothed actual power. RLS on the `[A, B]` pair with a 2×2
covariance matrix `P`. Constants (`model.go:19`):

```go
DefaultForgetting = 0.99   // ~100-cycle window
InitialCov        = 1000.0
OutlierSigma      = 5.0    // 5σ rejection after warmup
MinCommandForRLS  = 100.0  // W — skip low-signal cycles
SoCBucket         = 0.05   // 5 % SoC saturation buckets
```

Stability bounds `A ∈ [0.1, 0.99]`, `B ∈ [-1.5, 1.5]`
(`model.go:265`).

### Derived quantities

- Steady-state gain `k = B / (1 − A)` (`model.go:99`) — clamped to
  [0.3, 1.5] for display (`model.go:108`).
- Time constant `τ = −dt / ln(A)` (`model.go:120`), clamped to
  [0.05, 60] s.
- Saturation curves per SoC bucket — per-bucket max observed `|actual|`
  for charge and discharge, with slow decay (`SatDecay = 0.9999`) so
  stale over-optimistic peaks fade (`model.go:322`).
- Confidence in [0, 1] combining sample count (capped at 200) and
  residual variance (`model.go:135`).
- Health score in [0, 1] — gain drift vs a self-tune baseline; 0 = 50 %
  gain drift, 1 = baseline gain (`model.go:146`).

### Cascade controller

```go
// go/internal/battery/model.go:182
func (m *Model) Inverse(target float64) float64 {
    g := m.SteadyStateGainRaw()
    if math.Abs(g) < 0.3 || math.Abs(g) > 2.0 {
        return target // unhealthy model, pass through
    }
    return target / m.SteadyStateGain()
}
```

Outer PI computes the desired actual grid power. When confidence is
high, we invert through the model so the command we send produces the
desired actual on the next cycle (`UseCascade` toggle in
`go/internal/control/dispatch.go:84`). When the model is unhealthy
(gain out of [0.3, 2.0]) `Inverse` is a pass-through — PI command goes
direct.

### Persistence

JSON keyed on the resolved hardware-stable `device_id` (serial > MAC >
endpoint) via `state.Store.SaveBatteryModel(device_id, json)`
(`go/internal/state/store.go:265`). Save every 60 s. Legacy rows keyed
on driver name are migrated by `MigrateBatteryModelKeys`
(`go/internal/state/devices.go:111`) so battery history survives a
driver rename.

### Reset

`POST /api/battery_models/reset` (`go/internal/api/api.go:110`).

### Self-tune (`go/internal/selftune`)

Closed-loop step-response calibration:

1. Pause normal control.
2. Walk the state machine: stabilize → ±1000 W small → ±3000 W large →
   settle between each (`selftune.go:24`).
3. Fit τ + gain from the step response, write to
   `model.BaselineGain` / `BaselineTauS`, reseed `A, B` via
   `SetFromStepFit` (`model.go:310`).
4. Live drift afterwards is measured as gain deviation from baseline
   (`HealthScore`, `HealthDriftPerDay` from `GainHistory` linear
   regression).

Step commands are site-signed, so `+1000 W = charge 1 kW`, `−3000 W =
discharge 3 kW` (`selftune.go:57`).

---

## 5. MPC planner (`go/internal/mpc`)

Not a twin — the cost-minimizing search that consumes all four twins'
outputs and the current SoC. Covered in full in `docs/mpc-planner.md`;
here we only note the ML integration surface.

### Algorithm

Dynamic programming over a discretized SoC × action grid. Production
defaults (`go/cmd/ftw/main.go:696`):

- `SoCLevels = 41` (2.5 % steps across the SoC window).
- `ActionLevels = 21` (odd so battery-W = 0 is represented).
- Horizon = 48 h (`service.go:100`).
- Replan interval = 15 min.

Stress tests (`go/internal/mpc/stress_test.go`) use a 51 × 21 ×
96-slot high-resolution grid. Complexity is `O(N × S × A)`; ~100 k
state evaluations per solve, ~600 µs on a modern CPU.

### Strategies (`mpc.Mode`)

| Mode | Grid-charge? | Battery export? |
|---|---|---|
| `planner_self` / `ModeSelfConsumption` | no | no |
| `planner_cheap` / `ModeCheapCharge` | yes | no |
| `planner_arbitrage` / `ModeArbitrage` | yes | yes |

Enforcement in `modeAllows` (`go/internal/mpc/mpc.go:446`).

### Inputs

Per 15-min slot in the 48-h horizon:

- **Price** — `state.Store.LoadPrices(zone, since, until)`, extended by
  `extendPricesWithForecast` for the tail (`service.go:334`).
- **PV** — `PVPredictor = pvmodel.Service.Predict`
  (`service.go:478`), negated to site sign.
- **Load** — `LoadPredictor = loadmodel.Service.Predict`
  (`service.go:485`).
- **SoC** — `currentSoCPct` averaged across `telemetry.DerBattery`
  readings (`service.go:575`).
- **Efficiency** — per-direction, default 0.95 each → ~90 %
  round-trip (`mpc.go:95`).

### Per-slot export price

`Slot.SpotOre + ExportBonusOreKwh − ExportFeeOreKwh`, clamped to zero
(`mpc.go:203`). Previously a flat horizon-mean; that blinded the DP to
morning-vs-midday spreads. When `Params.ExportOrePerKWh > 0` a fixed
feed-in tariff overrides the per-slot value (`mpc.go:204`).

### Reactive replan

Leaky-integral divergence detector. Every `ReactiveInterval = 10 s`
the service compares live telemetry to the plan's current slot and
integrates the energy error with a 900-second (15-min) half-life
decay:

```go
// go/internal/mpc/service.go:266
decay := math.Pow(0.5, dtS / 900.0)
pvErrIntWh   = pvErrIntWh   * decay + (pvW - slot.PVW)       * dtH
loadErrIntWh = loadErrIntWh * decay + (loadW - slot.LoadW)   * dtH
```

Replan fires when `|pvErrIntWh| > 500 Wh` or
`|loadErrIntWh| > 400 Wh`, subject to a 60-second cooldown
(`service.go:104`). Both integrals reset to zero after firing.

### Curtailment flagging

`annotateCurtailment` (`mpc.go:408`) walks the plan and sets
`Action.PVLimitW` for slots where export is net-negative (fees exceed
revenue) *and* the battery is already charging at its max. The driver
dispatches the limit only if it advertises `supports_pv_curtail`.

### Annual-savings projection

`go/internal/mpc/stress_test.go` (scenario sweep +
`TestOptimizerPerformance`) runs synthetic year-long data through each
strategy and reports SEK saved vs the idle baseline per strategy.
Useful for regression catching when a planner tweak silently degrades
arbitrage.

---

## Where the models meet the controller

```
pvmodel.Predict \
loadmodel.Predict \                   mpc.Optimize → state.PlanTarget
priceforecast.Predict /       →  
telemetry.SoC /              

state.PlanTarget → control.PI (drives grid_w → grid_target_w)
control.PI → battery.Model.Inverse (cascade, when confidence > 0.5)
           → per-driver DispatchTarget
```

- `control.State.PlanTarget` is a `PlanTargetFunc` wired to
  `mpc.Service.GridTargetAt` in `main.go`.
- When the plan is older than `MaxPlanAge = 30 min` the control loop
  short-circuits to self-consumption with `grid_target_w = 0`
  (`go/internal/mpc/service.go:125`, `control/dispatch.go:161`).
- Watchdog: stale grid telemetry → control sends drivers to
  `default_mode`, drivers' autonomous logic takes over. Covered in
  `docs/safety.md`.

---

## Cold-start behaviour (day one)

| Twin | Day-0 output |
|---|---|
| PV | 100 % physics prior (`rated × clearsky/1000 × cloud_factor`). |
| Load | Baked Swedish-home prior, 168 buckets pre-seeded. |
| Price | Baked Nordic zone-aware shape + monthly multipliers. |
| Battery | PI direct (cascade off until `Confidence > 0.5`, ~100+ samples). |
| MPC | Runs day one with priors; improves as twins converge. |

---

## Persistence and reset paths

| Model | State key / storage | Reset endpoint |
|---|---|---|
| pvmodel | `state.Store.LoadConfig("pvmodel/state")` | `POST /api/pvmodel/reset` |
| loadmodel | `state.Store.LoadConfig("loadmodel/state")` | `POST /api/loadmodel/reset` |
| priceforecast | `state.Store.LoadConfig("pricefc/state")` | (no endpoint; auto-refits every 6 h) |
| battery | `state.Store.SaveBatteryModel(device_id, json)` | `POST /api/battery_models/reset` |

All endpoints are registered in `go/internal/api/api.go:108`–`122`.
