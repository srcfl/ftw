# mpc — receding-horizon battery scheduler

## What it does

Dynamic programming over a discretized SoC × action × slot grid. Turns price
forecast + PV forecast + load forecast + current SoC into a per-slot
`battery_w` schedule for the next 48h. Re-plans every 15 min on a ticker,
plus off-schedule whenever live PV/load drifts far enough from the current
slot's prediction.

## Math

Backward Bellman recursion over `V[t][s]` = min expected cost from slot `t`
onward starting at SoC bucket `s` (`mpc.go:246`):

```
V[t][s] = min over actions a of  cost(slot_t, a, s) + V[t+1][s']
```

State transition with efficiency (`mpc.go:264-274`):

```
a >= 0 (charge):    ΔSoC_Wh =  a * dt * charge_eff
a <  0 (discharge): ΔSoC_Wh =  a * dt / discharge_eff
```

Per-slot cost, confidence-blended toward horizon mean (`mpc.go:192-215`):

```
effPrice = c*raw + (1-c)*mean     (c = slot.Confidence)
cost = effPrice * gridKWh              if importing
cost = -(SpotOre + bonus - fee) * |gridKWh|   if exporting
```

Terminal credit for leftover SoC: `-TerminalSoCPrice * kWh_remaining`
(`mpc.go:240-243`), default = mean import price over horizon so the planner
is SoC-neutral.

Reactive replan: 15-min half-life leaky integral of `(actual - forecast)`
for PV and load (`service.go:266-290`). Trigger when `|pvErrIntWh| > 500 Wh`
or `|loadErrIntWh| > 400 Wh` (defaults at `service.go:104-105`), subject to
60s cooldown (`MinReplanGap`).

## Inputs / outputs

Inputs:

- `[]Slot` with `PriceOre` (consumer total), `SpotOre` (raw for export),
  `PVW` (site-sign, ≤ 0), `LoadW` (site-sign, ≥ 0), `Confidence` in [0,1].
- `Params`: mode, SoC bounds + grid size, action bounds + grid size,
  charge/discharge efficiency, terminal SoC price, export bonus/fee.
- Injected predictors (optional): `PVPredictor`, `LoadPredictor`,
  `PricePredictor`, all `service.go:18-28`.

Output: `Plan` — `Actions[]` with `BatteryW`, `GridW`, `SoCPct`,
`CostOre`, `PVLimitW` (curtailment, 0 = none), `Reason` (human string).

`Service.GridTargetAt(now)` returns `(gridW, true)` for the slot covering
`now`; `(0, false)` when plan missing, older than `MaxPlanAge = 30m`
(`service.go:125`), or out of horizon.

## Training cadence + persistence

Plan lives in memory only (`Service.last`, `service.go:87`). No persistence;
the next tick recomputes from fresh prices + model predictions. Re-plans on
`Interval` (15m default) or reactive trigger. Defaults per battery grid are
51 SoC × 21 action × 193 slots for 48h at 15-min resolution.

## Public API surface

Core optimizer (`mpc.go`):

- `Optimize(slots []Slot, p Params) Plan`
- `Mode` + constants `ModeSelfConsumption` / `ModeCheapCharge` / `ModeArbitrage`
- `IdleGateThresholdW` — constant consumed by `control` to decide whether
  a `planner_self` slot's `BatteryEnergyWh` counts as "DP picked idle".
  Mirrors the `chargeThresh` used by `reasonFor`; keep the two in sync.
- Types `Slot`, `Params`, `Action`, `Plan`.

Service (`service.go`):

- `New(st, tl, zone, defaults) *Service`
- `(*Service).Start(ctx)` / `.Stop()` / `.Latest()` / `.Replan(ctx)`
- `(*Service).GridTargetAt(now) (float64, bool)`
- `(*Service).SetMode(ctx, Mode)`
- `(*Service).LastReplanInfo() (time.Time, string)`
- Predictor hook types `PVPredictor`, `LoadPredictor`, `PricePredictor`.

## How it talks to neighbors

- `control/dispatch.go` injects `Service.SlotAt` as `PlanTargetFunc` and
  `Service.SlotDirectiveAt` as `SlotDirectiveFunc`. Control loop consults
  them each cycle; falls back to self-consumption when `(_, false)` is
  returned. For `planner_self` the dispatch reads `BatteryEnergyWh` out of
  `SlotDirective` only as an "idle-gate" signal (below `IdleGateThresholdW`
  average power ⇒ do not discharge, but absorb true live meter surplus);
  participant-slot power is still driven by reactive PI-on-gridW=0. For
  `planner_cheap` / `planner_arbitrage` the dispatch follows the Wh
  allocation directly. See issue #130 +
  `docs/plan-ems-contract.md`.
- Reads price history via `state.Store.LoadPrices` and forecast via
  `LoadForecasts` (`service.go:324,343`).
- Reads live PV / battery / meter via `telemetry.Store` for SoC + divergence
  checks (`service.go:242-260`).
- Optionally consumes `pvmodel.Service.Predict`, `loadmodel.Service.Predict`,
  `priceforecast.Service.Predict` via the injected func hooks.
- Operator-facing docs: `docs/mpc-planner.md`.

## What NOT to do

- Do not import pvmodel / loadmodel / priceforecast here — they're wired via
  function-typed fields in `Service` to avoid import cycles.
- Do not treat `Plan.TotalCostOre` as the DP objective: it is the
  raw-price re-scoring for the UI. The DP minimizes the confidence-blended
  objective (`mpc.go:350-365`).
- Do not remove the `MaxPlanAge` staleness check — the control loop relies
  on it to bail back to self-consumption if the planner has stalled.
- Do not feed forecasted (synthesized) prices with `Confidence = 1.0`;
  `buildSlots` marks `Source == "forecast"` rows with 0.6 (`service.go:489-494`).
