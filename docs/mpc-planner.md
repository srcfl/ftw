# MPC planner

The Model-Predictive-Control planner is the outer, slow loop that decides
*what the battery should do over the next 48 hours*. It runs every
15 minutes, emits a schedule of grid-power targets per 15-minute slot,
and feeds those targets into the inner 5-second control loop.

The planner never bypasses safety — fuse, SoC bounds, and slew limits
are enforced by the inner loop regardless of what the plan says.

---

## Strategies

Three user-selectable strategies. All respect SoC + power + efficiency
limits; the difference is **what the planner is allowed to do with the
grid connection**.

| Strategy | Grid-charge? | Battery export? | When to pick |
|---|---|---|---|
| **Self-consumption** | no | no | Safest. Battery covers local load and absorbs PV surplus without intentional battery export. |
| **Cheap charging** | yes (when cheap) | no | Good when export tariffs are low. Top up overnight, use during peaks. |
| **Arbitrage** | yes | yes | Biggest savings on volatile days. Charges cheap, discharges into expensive hours. |

The currently-selected strategy is shown in the `Strategy` control panel.
A one-sentence description refreshes every 5 seconds.

Legacy modes (`idle` / manual `self_consumption` / `peak_shaving` /
`charge`) remain available under the `Manual…` toggle.

### Execution differs by strategy

| Strategy | Dispatch layer |
|---|---|
| Self-consumption (Smart) | Reactive self-consumption + per-slot idle gate. The plan tells the EMS **whether** to spend SoC this slot — participant slots behave like manual self-consumption, while idle slots are charge-only and may absorb live meter surplus. |
| Cheap charging | Energy-allocation (default). Plan emits Wh-per-slot; EMS converts to W in real time; grid is the residual. See `docs/plan-ems-contract.md`. |
| Arbitrage | Same as Cheap charging — energy-allocation. |

When the plan is stale (> 30 min old) or absent, every strategy falls
back to manual self-consumption. Operators see `plan_stale: true`
in the status endpoint.

Under Self-consumption, a slot where the optimizer allocated `|battery_energy_wh|
/ slot_hours < 100 W` (avg) is interpreted as **idle**: the EMS will not
discharge to cover live import, but it still charges from true live PV
surplus that would otherwise cross the site meter. Any larger allocation
flips the slot to **participate** — i.e. behave exactly like manual
self-consumption: charge live surplus, or discharge to cover live import,
without intentionally exporting via the battery.

---

## How it thinks

For each slot in the 48-hour horizon, the planner receives:

- **price** — consumer öre/kWh (spot + grid tariff + VAT)
- **PV forecast** — expected generation W (digital twin)
- **load forecast** — expected household consumption W (digital twin)
- **SoC at start of slot** — propagated from the previous slot's decision
- **confidence** — 1.0 if day-ahead real, ~0.6 if ML-forecasted
- **asset state and limits** — each online battery and scheduled loadpoint
- **site limits and forecast scenarios** — fuse/export ceilings plus PV risk

CVXPY builds one sparse LP/MILP across the horizon. Battery power and SoC are
continuous; EV charger steps and other real operating modes are integer
decisions. HiGHS solves the primary model, while CLARABEL can solve a
continuous convex formulation. See [optimizer.md](optimizer.md).

### Cost function

```
slot_cost = import_cost − export_revenue
          = price × max(grid_kWh, 0) − export_price × max(−grid_kWh, 0)
```

where `grid = load + pv + battery`, all site-signed (import positive,
PV negative, battery positive = charging). Export revenue defaults to
`mean_spot + export_bonus − export_fee`.

### Confidence blending (the "don't bet the farm on a guess" lens)

Predicted slots are less trustworthy than real day-ahead prices. The optimizer
doesn't ignore them — it pulls them toward the horizon mean:

```
effective_price[t] = confidence × real_price + (1 − confidence) × horizon_mean
```

With `confidence = 0.6`, a predicted 200 öre peak is weighted as if it
were only about 160 öre for the decision, while a predicted 20 öre trough
gets pulled up to about 60 öre. Result: the planner still commits when
the ML forecaster *and* the hour-of-week pattern agree, but it doesn't
lock in expensive arbitrage trades on a prediction that may be off.

The reported `cost_ore` on each action is the **raw** (un-blended) value
— what you'd actually pay if prices hold. Blending is a decision lens
only.

### Per-slot decision reasons

Every slot comes with a short human-readable `reason` string, surfaced
in the UI hover tooltip:

- `absorb PV surplus` — battery charging, PV-surplus baseline
- `charge — price below horizon mean` — optimizer picked up on a cheap slot
- `discharge — cover local load` — battery covering house consumption
- `discharge — price above horizon mean` — optimizer discharging into a peak
- `idle — import to cover load`
- `idle — export PV surplus`

Predicted slots have `(predicted)` appended.

---

## Horizon

Default: **48 hours**. Day-ahead auctions usually publish tomorrow's
prices around 13:00 CET; before that, the price-forecaster fills the
gap so the horizon is always 48 h regardless of when you look.

Configurable via `planner.horizon_hours`.

---

## Staleness guard

If the most recent plan is older than `MaxPlanAge = 30 min`, the control
loop falls back to `self_consumption` with `grid_target_w = 0`. The
status endpoint exposes `plan_stale: true` so the UI can warn.

A single missed replan is absorbed without fallback (the window is
double the replan interval). Only sustained outage trips it.

---

## Dispatch path

```
15-min cadence:
  MPC.replan() → Plan cached on Service
                   ↑
                 price forecaster + PV twin + load twin

5-second cadence:
  Control.dispatch():
    if Mode.IsPlannerMode():
        grid_target_w = plan.GridTargetAt(now)   // current-slot lookup
    PI controller chases grid_target_w
    Cascade splits across batteries (proportional / weighted / priority)
    Fuse + SoC clamps
```

The control loop keeps running `self_consumption` logic when no
planner strategy is selected — the MPC only writes `grid_target_w`
when the operator has chosen one.

---

## API surface

- `GET  /api/mpc/plan` — latest cached plan (mode, horizon, per-slot actions with reasons)
- `POST /api/mpc/replan` — force an immediate replan
- `GET  /api/mpc/diagnose` — per-slot audit view plus solver metadata and the exact versioned optimizer input used for deterministic replay
- `POST /api/mode {"mode":"planner_arbitrage"}` — activates a strategy AND forces an MPC replan so targets take effect within one control cycle
- `GET  /api/loadpoints` — list configured EV loadpoints + observable state (plug, SoC, power, session Wh)
- `POST /api/loadpoints/{id}/target` — set user intent `{soc_pct: 80, target_time_ms: …}`; triggers an MPC replan

---

## Tuning knobs

| Config | Default | Effect |
|---|---|---|
| `planner.mode` | `self_consumption` | Starting strategy |
| `planner.horizon_hours` | 48 | How far ahead to plan |
| `planner.interval_min` | 15 | Replan cadence |
| `planner.soc_min_pct` / `soc_max_pct` | 10 / 95 | SoC bounds the optimizer respects |
| `planner.charge_efficiency` / `discharge_efficiency` | 0.95 / 0.95 | Round-trip = 0.9025 |
| `planner.export_ore_per_kwh` | mean spot | Export revenue per kWh |
| `planner.engine` | `python` | Primary engine; `dp` is rollback |
| `planner.optimizer_formulation` | `auto` | Automatic convex/MILP selection |
| `planner.optimizer_timeout_s` | 5 | Whole worker request deadline |
| `planner.optimizer_mip_rel_gap` | 0.005 | Accepted HiGHS relative MIP gap |
| `planner.optimizer_cvar_weight` | 0.15 | Tail-risk weight; explicit 0 disables |

The former fixed SoC/action grids no longer constrain the primary plan.

---

## When arbitrage won't help

Flat-price days give the optimizer little timing value; all three strategies
return near-identical schedules. See `go/internal/mpc/stress_test.go`
for the scenario-comparison benchmark that illustrates this.

---

## Solar curtailment

Post-processing flags each plan slot with a recommended PV cap
(`pv_limit_w`) when all three conditions hold:

1. the slot is exporting (`grid_w < 0`)
2. export revenue is non-positive (spot ≤ 0 or fees ≥ revenue)
3. local consumption (load + battery charge + any planned EV loadpoint
   charge) cannot absorb the PV

In that regime, exporting costs money and curtailing the PV is a pure
win. The recommended limit equals `load_w + max(battery_w, 0) + max(loadpoint_w, 0)`
— just enough to cover on-site absorption (including EV charging the
planner itself scheduled).

The MPC cost function isn't changed by curtailment; the annotation is
a dispatch-time suggestion. A driver that advertises
`supports_pv_curtail` can pick up `action.pv_limit_w` on each control
cycle and send a setpoint to the inverter. Drivers without this
capability ignore the field; curtailment then just doesn't happen.

---

## Savings — what to expect

The stress test (`go test -run AnnualSavings ./internal/mpc/`) projects
annual SEK savings across six scenario types weighted by prevalence:

| Scenario | Weight | Self-cons | Cheap-charge | Arbitrage |
|---|---:|---:|---:|---:|
| sunny_mild | 25% | −55 | −51 | 63 |
| cloudy | 30% | 523 | 1 010 | 1 088 |
| price_spike | 5% | 753 | 2 239 | 2 388 |
| flat_prices | 10% | 116 | 117 | 121 |
| cheap_night | 15% | 602 | 1 278 | 1 726 |
| solar_surplus | 15% | −504 | −504 | −421 |
| **TOTAL SEK/yr** | | **1 435** | **4 088** | **4 964** |

Figures based on a 15 kWh / 5 kW battery with **per-slot export
pricing** (export earns the current slot's spot, not a horizon
average — reflects the real Nordic setup).

Signal to read from the table:

- **Arbitrage dominates volatile days** (price_spike, cheap_night)
  where the battery's job is genuinely "buy low, sell high".
- **Self-consumption can lose money on high-PV-surplus days** —
  cycling the battery through round-trip losses to export at
  midday's low spot is a net loss vs. just exporting PV directly.
- **Cheap-charge + Arbitrage** close the gap on those days because
  they're smart enough to _not_ cycle unnecessarily.

Total annual savings depend heavily on market volatility; a stable
market year narrows the gap between the three modes.

---

## Time-zone convention — UTC everywhere

All time-of-day and day-of-week indexing inside the planner and its
digital twins is done in **UTC**. The price-forecast's hour-of-week
buckets, the load-model's hour-of-week buckets, the PV-twin's
time-of-day harmonic features, and every `time.UnixMilli(...)`
conversion that feeds a predictor all coerce to UTC before the
`.Hour()` / `.Weekday()` / `.Month()` access.

Why: a wall-clock 19:00 in Stockholm lands on a *different* UTC hour
in summer (CEST, UTC+2) than in winter (CET, UTC+1). Indexing buckets
by local-zone hour silently slides the learned EMA by one bucket
twice a year, and a single `Predict` call could resolve a different
bucket for the same instant depending on which `time.Location` the
`time.Time` carried. Both bugs produce "planner chose to charge from
an expensive hour" symptoms around DST transitions.

Source-of-truth timestamps in the state store are unix-milli (absolute,
timezone-agnostic); the UTC coercion is only at the leaf points where
we access calendar fields. Operator-facing UI formatting still uses
the site's local zone — that's a display concern, not a model
concern.

If you add a new predictor or new time-field access inside the
planner, coerce `t` with `t.UTC()` first. Tests
(`TestPredictStableAcrossDST`, `TestHourOfWeekStableAcrossDST`,
`TestFeaturesStableAcrossDST`) enforce the invariant per package.

---

## Unified EV charging (Phase 4)

When one or more loadpoints are configured and scheduled, the mathematical
model adds one energy state and one power decision set per vehicle. The
optimization then schedules **battery AND EV actions
jointly** rather than treating the EV as uncontrolled load — avoiding
the "two planners fighting" problem that happens when an external EVCC-
style scheduler runs alongside our MPC.

The charger's allowed W levels are modeled with one-hot MILP variables. Adding
vehicles grows variables and constraints linearly instead of multiplying a DP
state space.

### Target + deadline

User intent (`POST /api/loadpoints/{id}/target`) sets `target_soc_pct` +
`target_time_ms`. The planner maps the time to a horizon slot index
and adds a non-negative shortfall variable at that slot. A first solve
minimizes service shortfall, then locks that result before minimizing energy
cost. Infeasible targets therefore maximize delivered energy without relying
on a tuned monetary penalty.

### No deadline

`target_soc_pct == 0` or `target_time_ms == 0` means "charge
opportunistically". No penalty, no push — the optimizer will only charge when
a slot is cheap enough to be worth it relative to battery alternatives
and terminal credit.

### Output

`Action.LoadpointW` per slot (W, positive = charging) and
`Action.LoadpointSoCPct` (post-slot EV SoC). The SlotDirective the EMS
consumes carries `LoadpointEnergyWh[id]` and `LoadpointSoCTargetPct[id]`
for the dispatch layer.

### Dispatch

Per control tick (5 s) the main loop:

1. Reads each loadpoint's driver telemetry (`tel.Get(driver, DerEV)` →
   `{connected, session_wh}`).
2. Calls `lpMgr.Observe(id, connected, powerW, sessionWh)`. The
   manager infers vehicle SoC as `plugin_soc_pct + session_wh /
   vehicle_capacity * 100` (chargers like Easee don't expose the
   BMS).
3. Fetches `SlotDirectiveAt(now).LoadpointEnergyWh[id]` from the
   MPC.
4. Computes `remainingWh / remainingSeconds → W` (same
   energy-allocation contract as the battery), snaps to
   `AllowedStepsW`, and sends
   `{"action":"ev_set_current","power_w": W}` to the loadpoint's
   driver.

The Easee driver divides `power_w` by configured phases,
clamps to 6-32 A, and sets `dynamicChargerCurrent` via the cloud
API. 0 W cleanly pauses the session.

### What's still outstanding

- Per-loadpoint divergence check for reactive replan
- UI for setting target + target-time (API is ready; web-form follows)
- Phase-switching heuristic inside the Easee driver for sub-3 kW
  surplus windows
