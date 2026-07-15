# Battery Models — online learning, cascade control, hardware health

This is what makes FTW adaptive: every battery has its own learned
model of how it actually responds to power commands. The system uses these
models in three ways:

1. **Cascade control** — each battery gets its own inner PI loop that compensates
   for its specific response time, efficiency, and saturation characteristics.
2. **Inverse model** — commands are pre-compensated for the battery's measured
   gain so the *actual* power matches the *target* power.
3. **Hardware health** — gain drift over time signals battery aging or hardware
   degradation before it becomes catastrophic.

## The model: ARX(1)

Each battery is described by a first-order autoregressive model:

```
y(t+1) = a · y(t) + b · u(t) + ε
```

Where:
- `u(t)` = power command we sent (W)
- `y(t)` = observed battery power one cycle later (W)
- `a` ∈ [0.001, 0.99] = "memory" coefficient (how much of the previous state persists)
- `b` ∈ [-1.5, 1.5] = control influence

From these two parameters we derive the physically meaningful quantities:

| Quantity | Formula | Meaning |
|---|---|---|
| Time constant `τ` | `−Δt / ln(a)` | Seconds to reach ~63% of a step change |
| Steady-state gain `k` | `b / (1 − a)` | Actual power / commanded power at equilibrium |

Examples (with `Δt` set to the current control interval):

| `a` | `b` | `τ` | `k` | Interpretation |
|---|---|---|---|---|
| 0.6 | 0.4 | 9.8s | 1.00 | Slow but lossless |
| 0.5 | 0.42 | 7.2s | 0.84 | Slow with 16% loss |
| 0.04 | 0.86 | 1.6s | 0.90 | Fast (Ferroamp-like) |
| 0.04 | 0.95 | 1.6s | 0.99 | Fast and lossless |

A typical lithium battery + inverter has `k ≈ 0.85–0.97` (efficiency loss) and
`τ ≈ 1–4s` (depending on inverter control loop + comms latency).

## How learning works

### Continuous learning — Recursive Least Squares (RLS)

Every control cycle we have a fresh `(u(t-1), y(t-1), y(t))` triple. Standard
RLS update:

```
φ  = [y(t-1), u(t-1)]                  # regressor
ŷ  = a · φ[0] + b · φ[1]              # prediction
e  = y(t) − ŷ                          # residual
K  = P · φ / (λ + φᵀ · P · φ)         # Kalman gain
[a, b] ← [a, b] + K · e                # parameter update
P  ← (P − K · φᵀ · P) / λ              # covariance update
```

`λ = 0.99` is the **forgetting factor** — gives an effective window of ~100
samples, so old data slowly fades. This makes the
model adapt to gradual changes (battery aging, ambient temperature) without
being too noisy.

### Data quality gating

RLS happily fits noise into garbage parameters if you feed it junk. We apply
three filters before each update:

1. **Min command magnitude** — if `|u| < 100W`, the input signal is too small
   to be informative. Skip.
2. **Min state change** — if `|y(t) − y(t-1)| < 20W` (after warmup), the system
   is at steady state and adds no info. Skip.
3. **Outlier rejection** — if residual `|e| > 5σ` (where σ comes from the EMA
   of squared residuals), the observation is likely a disturbance, not a model
   error. Skip.

Plus hard bounds on `a` and `b` to prevent divergence under pathological data.

### Saturation curves

Separate from the ARX model, we track the **maximum observed actual power**
per 5%-SoC bucket. This gives an empirical envelope:

```
max_charge_w(SoC) — how much we can actually push at this SoC
max_discharge_w(SoC) — how much we can actually pull
```

Updated each cycle: `curve[bucket] = max(observed, curve[bucket] · 0.9999)`.
The decay factor lets old over-optimistic peaks fade slowly so we converge to
sustainable values. After a few days of operation we have a real-world derating
curve that knows your battery's BMS quirks better than any spec sheet.

## Cascade control

Without per-battery models, a single site-level PI tells each battery "I want
you to discharge X watts" and assumes that's what happens. With models, the
control flow is:

```
   site_error ──► PI_site ──► total_correction
                                    │
                                    ▼
                           split_proportional
                                    │
                ┌───────────────────┴───────────────────┐
                ▼                                       ▼
        ferroamp_target                         sungrow_target
                │                                       │
       ╔════════╧════════╗                     ╔════════╧════════╗
       ║ saturation_clamp(SoC)                 ║ saturation_clamp(SoC)
       ║ inner_PI(target − actual)             ║ inner_PI(target − actual)
       ║ inverse(command / gain)               ║ inverse(command / gain)
       ╚════════╤════════╝                     ╚════════╤════════╝
                ▼                                       ▼
        ferroamp_command                        sungrow_command
```

Each per-battery block:

1. **Saturation clamp** — never asks for more than the SoC curve says is possible.
   Sets `clamped` flag for visibility.
2. **Inner PI** — auto-tuned from learned `τ`. Faster batteries get tighter Kp:
   ```
   Kp = clamp(1 / (τ + 0.5), 0.2, 1.0)
   Ki = clamp(0.2 / max(τ, 0.5), 0.05, 0.4)
   ```
3. **Inverse model** — `command = pi_corrected_target / steady_state_gain`.
   So if a battery has `k = 0.85`, asking for −1000W actual sends a command of
   −1176W (1000 / 0.85), which the battery's internal control loop converts into
   the desired actual output.

Falls back gracefully:
- No model yet? → use raw target
- Low confidence (`< 0.3`)? → don't re-tune inner PI
- Implausible learned gain (`< 0.3` or `> 2.0`)? → inverse model passes through

## Self-tune (manual calibration)

Online RLS is good but slow — it needs varied control activity to converge. The
self-tune button runs a **deliberate step-response sequence** that produces
clean data in 3 minutes per battery.

### Sequence per battery (~135s)

| t (s) | Step | Command | Why |
|---|---|---|---|
| 0–15 | Stabilize | 0W | Settle from previous activity |
| 15–30 | Step UP small | +1000W | Measure τ + gain at low magnitude |
| 30–45 | Settle | 0W | Reset |
| 45–60 | Step DOWN small | −1000W | Same, opposite direction |
| 60–75 | Settle | 0W | |
| 75–95 | Step UP large | +3000W | Probe upper saturation |
| 95–105 | Settle | 0W | |
| 105–125 | Step DOWN large | −3000W | Probe lower saturation |
| 125–135 | Fit + write model | — | |

### Fitting the step response

For a first-order step `y(t) = k·u·(1 − exp(−t/τ))`:
- `k` = mean of the last 30% of samples / commanded value (steady-state estimate)
- `τ` = time at which `y` crosses 63.2% of `(steady_state − initial)`

We fit each of the 4 active steps independently and average the valid fits.
Steps that produced `< 50W` response are rejected (battery may have been at
saturation, deadband, offline, or simply not responding).

### Baseline + hardware health

When a self-tune completes, we save the fitted `(gain, τ)` as the
**baseline_gain** and **baseline_tau_s** in `BatteryModel`. Later online RLS
updates compare against this baseline to compute:

```
health_score = clamp(1 − 2·|current_gain − baseline_gain| / |baseline_gain|, 0, 1)
```

So a 25% drift in gain → health = 0.5. A 50% drift → health = 0. The factor of 2
makes the score conservative — batteries should not drift much in normal
operation, so 10% drift already shows as health = 0.8.

### Drift per day (trend)

We keep a rolling window of `(timestamp_ms, gain)` pairs (last 2000 RLS
updates). Linear regression on this gives the slope in **gain per millisecond**,
converted to `gain_per_day`. Negative = degrading.

This is the early-warning signal: if `health_drift_per_day < −0.005` (gain
falling 0.5%/day) over a week, something is changing and worth investigating.

## API

### `GET /api/battery_models`

```json
{
  "ferroamp": {
    "tau_s": 1.42,
    "gain": 0.93,
    "deadband_w": 47,
    "n_samples": 1240,
    "confidence": 0.87,
    "health_score": 0.96,
    "health_drift_per_day": -0.0012,
    "baseline_gain": 0.95,
    "baseline_tau_s": 1.40,
    "last_calibrated_ts_ms": 1776103070123,
    "last_updated_ts_ms": 1776103245000,
    "max_charge_curve": [[0.0, 4980], [0.85, 4900], [0.95, 1820], [1.0, 0]],
    "max_discharge_curve": [[0.10, 800], [0.20, 4500], [1.0, 5000]],
    "a": 0.04,
    "b": 0.89
  }
}
```

### `POST /api/self_tune/start`

```json
{ "batteries": ["ferroamp", "sungrow"] }
```

Returns `{"status":"started", "batteries":[...]}` or `409` if a tune is already
running.

### `GET /api/self_tune/status`

```json
{
  "active": true,
  "battery_index": 0,
  "battery_total": 2,
  "current_battery": "ferroamp",
  "current_step": "step_up_small",
  "step_elapsed_s": 8.4,
  "total_elapsed_s": 23.4,
  "before": { "ferroamp": { "gain": 0.91, "tau_s": 1.8, ... } },
  "after":  { },
  "last_error": null
}
```

### `POST /api/self_tune/cancel`

Aborts an in-progress tune, restores normal control. No body required.

## Persistence

Models live in SQLite (`state.db` by default) in the `battery_models` table,
keyed by stable `device_id` rather than the configured driver name. That keeps
trained models attached to the physical device when a driver is renamed in
YAML, as long as the driver reports stable make/serial identity. On startup,
models are loaded back so RLS state survives restarts.

## Calibration UI

The dashboard keeps battery model internals out of the normal driver cards.
Most operators only need to know whether the driver is online and tracking its
target; τ, gain, sample count, and cascade/direct state stay internal unless
you inspect the API directly.

The **Self-tune** button opens a modal with:
- Pre-tune: checklist of batteries + a warning about ideal conditions (low PV,
  stable load, SoC 30–70%)
- During tune: live step name, per-step progress bar, overall progress bar,
  elapsed time, ETA
- Post-tune: before/after diff table for τ and gain per battery, with green/red
  delta indicators

## Failure modes

| Symptom | Likely cause | Fix |
|---|---|---|
| `confidence` stays low | Battery not active enough — RLS rarely sees usable samples | Run a self-tune to seed the model |
| `gain` drifts toward bound (0.3 or 1.5) | RLS struggling, possibly meter noise too high | Check telemetry; lower forgetting factor; run self-tune |
| Self-tune step shows "no usable step responses" | Battery offline / saturated / commands within deadband | Check SoC (must be 30–70%), verify battery accepts commands |
| `health_score` drops over weeks | Real battery degradation | Replace cells / inspect inverter |
| Inner PI oscillates | `τ` estimate is wrong (model not converged) | Run self-tune; if needed, disable the cascade path in code while investigating |

## When NOT to trust the model

- `confidence < 0.3` → cascade still runs but inner PI doesn't auto-retune
- `|raw_gain| < 0.3` or `> 2.0` → inverse model falls back to passthrough
- Self-tune fitted no valid steps → baseline NOT updated, `last_error` set

The cascade is designed to **degrade gracefully**: when models are missing or
implausible, the system behaves identically to the old direct-command mode. You
can also force-disable the cascade path in code while debugging.
