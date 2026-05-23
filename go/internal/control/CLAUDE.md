# control — site-level PI + dispatch modes + slew + fuse guard

## What it does

One cycle of site closed-loop control: reads the site meter from `telemetry`, runs an outer PI toward `GridTargetW`, splits the correction across online batteries by the active mode's distribution rule, applies per-driver slew + SoC + per-command clamps, then a global fuse guard. Returns a slice of `DispatchTarget` — the caller (main.go) is responsible for actually sending them to drivers. Entirely site-signed (see `../../../docs/site-convention.md`); no sign flips happen here. A holdoff timer (`MinDispatchIntervalS`, default 5 s) suppresses dispatch when the previous cycle ran less than N seconds ago, preventing command-spam when the control interval is shorter than the battery's response time.

## Key types

| Type | Purpose |
|---|---|
| `State` | All per-cycle persistent state (mode, setpoint, PI, slew memory, previous targets, planner hook). |
| `Mode` | String enum: `idle`, `self_consumption`, `peak_shaving`, `charge`, `priority`, `weighted`, `planner_{self,cheap,arbitrage}`. |
| `DispatchTarget` | `{Driver, TargetW, Clamped}` — one command per battery. |
| `PIController` / `PIOutput` | 2-term controller with anti-windup on the integral. |
| `PlanTargetFunc` | Callback into `mpc` — injected by main, returns `(mode_string, grid_target_w, ok)` for the current slot. The `mode_string` maps to a `Mode` constant so the plan can switch the EMS strategy per slot. |
| `PlanStale` | `bool` field on `State` — set `true` when a planner-mode cycle falls back to self_consumption because the plan was missing or stale (>30 min). Surfaced via the API for the UI. |
| `batteryInfo` | Internal per-cycle snapshot of a battery (capacity, current W, SoC, online). |

## Public API surface

- `NewState(gridTargetW, gridToleranceW, siteMeter)` — defaults match the Rust port: `PI(Kp=0.5, Ki=0.1, iLim=3000, outLim=10000)`, slew 500 W, holdoff 5 s, peak 5 kW.
- `(*State).SetGridTarget(w)` — updates both `GridTargetW` and the PI setpoint atomically.
- `ComputeDispatch(store, state, driverCapacities, fuseMaxW)` — the one function main.go calls every cycle.
- `NewPI(kp, ki, iLimit, outputLimit)` / `(*PIController).Update / Reset` — exposed for tests and any non-site PI use.
- Mode constants: `ModeIdle`, `ModeSelfConsumption`, `ModePeakShaving`, `ModeCharge`, `ModePriority`, `ModeWeighted`, `ModePlannerSelf`, `ModePlannerCheap`, `ModePlannerArbitrage`; `(Mode).IsPlannerMode()`.

Distribution (`distributeProportional`, `distributePriority`, `distributeWeighted`, `chargeAll`) and clamp helpers (`applyFuseGuard`, `clampWithSoC`) are unexported — tests reach them via `ComputeDispatch`.

## How it talks to neighbors

Imports `telemetry` and `mpc` (only for the `IdleGateThresholdW` constant — no behaviour, no types). Reads via `store.Get` (site meter, batteries), `store.ReadingsByType(DerPV)` (fuse guard), and `store.DriverHealth` (online check). **Does not call drivers** — `main.go` takes the returned `[]DispatchTarget` and forwards to the driver registry. `State.PlanTarget` / `State.SlotDirective` are the only upward callbacks; main wires them to `mpc` so this package stays planner-agnostic at the behaviour level. Consumers that mutate `State`: `api` (mode + grid_target changes), `ha.bridge` (Home Assistant select), `configreload.watcher` (YAML reload), `e2e` tests.

## What to read first

`dispatch.go` — `ComputeDispatch` (line 139) is the whole story in one function: planner override → idle/charge short-circuits → holdoff → read grid → pick error by mode → PI → distribute → slew → fuse guard. `pi.go` is ~60 lines; read it if you suspect integral windup.

## Planner execution paths

Three different execution strategies depending on the operator-selected planner mode — keep these straight:

| Mode | Execution | Why |
|---|---|---|
| `planner_self` | Reactive self-consumption + per-slot idle gate derived from `SlotDirective.BatteryEnergyWh` vs `mpc.IdleGateThresholdW`. | The mode's contract is "never grid-charge and never export via the battery". Participant slots may discharge to cover local import; idle slots are charge-only and may absorb true live meter surplus, while idle/charge slots floor negative targets. See issue #130 + `docs/plan-ems-contract.md` §"Exception: planner_self". |
| `planner_cheap` / `planner_arbitrage` w/ `UseEnergyDispatch=true` (default) | Energy-allocation: `targetTotalW = remaining_wh × 3600 / remaining_s`. Grid is the residual. | The whole point of these modes is to cycle the battery across the zero-grid line on purpose. Wh-per-slot is the optimisation variable. |
| `planner_cheap` / `planner_arbitrage` w/ `UseEnergyDispatch=false` (opt-out) | Legacy PI chasing `grid_target_w` returned by `PlanTarget`. | Emergency rollback only. |

Stale/missing plan → fall back to manual self_consumption with `grid_target=0` (identical across all planner modes).

## What NOT to do

- **Do NOT expect planner_cheap / planner_arbitrage modes to stay distinct inside `ComputeDispatch`.** They collapse to `self_consumption` right after setting `grid_target` from the plan (dispatch.go legacy branch). The operator-visible mode is preserved on `state.Mode`; the local `effectiveMode` is what drives behaviour.
- **Do NOT switch `planner_self` back to the energy-allocation path.** That would re-introduce issue #130: the battery follows planned Wh blindly and crosses the zero-grid line whenever the forecast is wrong. The test `TestPlannerSelfReactsToForecastOverestimate` is the regression guard.
- **Do NOT switch idle-gated `planner_self` into full self-consumption.** The idle gate is intentionally charge-only: compute true live meter surplus with current battery power removed, absorb that surplus if it exceeds the threshold, and otherwise drive the battery toward 0. It must not discharge on live import until the planner marks the slot as participating or the plan goes stale. Regression guards: `TestPlannerSelfIdleGateAbsorbsLargeLiveSurplus`, `TestPlannerSelfIdleGateDoesNotTreatBatteryDischargeAsSurplus`, `TestPlannerSelfIdleGateHoldsDuringImport`.
- **Do NOT distribute from the delta.** `distributeProportional` + `distributeWeighted` split the TOTAL desired site battery power (`currentTotal + totalCorrection`), not the correction alone. This is the fix for the "batteries drift apart" bug — keep it that way (dispatch.go:317, 368).
- **Do NOT drop the `groupPV` argument on `distributeProportional`.** When `State.InverterGroups` is non-empty the function routes charging first to the battery whose inverter also has live PV output (DC-local), then overflows proportionally (#143). Passing `nil`/empty disables the locality bonus and falls back to the pure capacity-proportional split — safe but suboptimal on multi-inverter sites.
- **Do NOT hard-code `MaxCommandW` at clamp points.** `clampWithSoC` takes a `batteryInfo` carrying per-driver `maxChargeW` / `maxDischargeW` (see `State.DriverLimits`, #145). The constant is the fallback when the driver didn't override. The post-slew re-clamp and `chargeAll` do the same lookup via `State.DriverLimits`.
- **Do NOT weaken `applyFuseGuard` to a single direction.** The guard predicts grid flow after targets and scales EITHER charge (when import → over fuse) OR discharge (when export → over fuse). That bidirectional protection is the operator's guarantee that the fuse is the non-negotiable ceiling (#145). The pre-#145 version only scaled discharge; don't go back.
- **Do NOT issue charge commands when SoC < 5 % and target < 0.** `clampWithSoC` (dispatch.go:410) blocks discharge of an empty battery. Per-command cap is hard-coded to 5 kW.
- **Do NOT flip signs.** Everything in this package is site convention: `+` = charge (import), `−` = discharge (export). `applyFuseGuard` reads `|battery discharge| + |PV|` as total generation; that's correct because PV is always `−` at the meter.
- **Do NOT call drivers from here.** Return `[]DispatchTarget` and let main's driver registry own the actuation. Keeps the dispatcher test-friendly (no I/O) and preserves the one-way data flow telemetry → control → main → drivers.
