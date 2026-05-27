# T001 Scout receipt — dispatch flow + existing clamps

## Summary

The plan's battery target reaches dispatch via
`control.ComputeDispatch` in `go/internal/control/dispatch.go`. The new
clamp should sit **after `applyPlanSignFloor` (line 953) and before
`forceFuseDischarge` (line 963)** so it cannot accidentally cancel the
reactive fuse-saver. The live meter is already read once as
`rawGridW = store.Get(SiteMeterDriver, DerMeter).SmoothedW`
(`dispatch.go:574-577`), site-signed (positive = import).
`GridToleranceW` defaults to **42 W**, currently used only as the
legacy PI deadband at `dispatch.go:763`. None of the existing seven
clamps cover the "load-twin over-prediction → grid export" case.

## Dispatch flow

| Step | file:line | what happens |
|---|---|---|
| ticker tick | `go/cmd/forty-two-watts/main.go:1364` | `WatchdogScan` flips stale drivers offline and sends `DefaultMode` |
| site-meter stale short-circuit | `main.go:1383-1391` | `tel.IsStale(SiteMeterDriver, DerMeter, watchdogTimeout)` → `SendDefault` to every driver, `continue` (skip dispatch). **The new clamp is never reached when meter is stale.** |
| `ComputeDispatch` entry | `dispatch.go:391` | pulls planner mode + (optional) `SlotDirective` / `PlanTarget`, resolves `effectiveMode` |
| manual_hold override | `dispatch.go:438-453` | forces `ModeSelfConsumption`, resets PI + slot accumulators |
| idle / charge short-circuits | `dispatch.go:527-548` | `ModeIdle` → `fuseSaverFromZero`; `ModeCharge` → `chargeAll` |
| holdoff | `dispatch.go:554-571` | enforce `MinDispatchIntervalS` (default 5 s) |
| read site meter | `dispatch.go:573-577` | `rawGridW = store.Get(SiteMeterDriver, DerMeter).SmoothedW` (site-signed) |
| EV subtraction | `dispatch.go:598-601` | `gridW = rawGridW - state.EVChargingW` (unless `BatteryCoversEV`) |
| gather online batteries | `dispatch.go:603-639` | builds `onlineBats` from `driverCapacities` + `DerBattery` readings |
| compute `totalCorrection` | `dispatch.go:649-780` | branches on `manualHold` / `plannerSelfIdleGate` / `useEnergyPath` / default-legacy |
| legacy PI path | `dispatch.go:739-779` | `errW = gridW - GridTargetW`; deadband `|errW| < GridToleranceW` returns nil; then `PI.Update` → `totalCorrection` |
| joint fuse allocator | `dispatch.go:809-831` | scales charge + EV proportionally if projected > `fuseMaxW` |
| distribute | `dispatch.go:850-858` | `distributeProportional/Priority/Weighted` splits `totalCorrection` |
| slew per driver | `dispatch.go:876-894` | anchor = `SmoothedW`; clamp delta to ±`SlewRateW` (default 500 W/cycle) |
| post-slew re-clamp | `dispatch.go:903-921` | per-driver `MaxChargeW` / `MaxDischargeW` (fallback `MaxCommandW = 5000`) |
| `applyFuseGuard` | `dispatch.go:924` (impl `1290-1338`) | `predicted = currentGrid - currentBat + sumTarget`; scale charges/discharges if `±fuseMaxW` exceeded |
| `applyPlanSignFloor` | `dispatch.go:953` (impl `1596-1628`) | if plan intent and sum(targets) disagree in sign (±100 W band), zero all targets |
| `forceFuseDischarge` | `dispatch.go:963` (impl `1750+`) | recomputes predicted with live meter; if > `effFuseW` forces extra discharge ignoring slew |
| bookkeeping | `dispatch.go:1003-1010` | `state.LastDispatch=now`, `PrevTargets` updated, `LastTargets` stored |
| driver send | `main.go:1419-1424` | `reg.Send(ctx, t.Driver, {"action":"battery","power_w":t.TargetW})` |

## Key locations

### `applyPlanSignFloor`
- decl: `dispatch.go:1596`
- called at: `dispatch.go:953`
- threshold constant: `dispatch.go:1608` — local `const idleBand = 100.0`
- intent source: `dispatch.go:1634-1665` (`planSignIntent`) reads `SlotDirective` or `PlanTarget`; uses local consts `idleWh=50.0`, `idleGridW=100.0`
- effect: zeroes all targets for the tick when plan-intent and exec sum disagree in sign

### `GridToleranceW`
- yaml field: `go/internal/config/config.go:199`
- default: `config.go:698-699` → **42 W**
- validation: `config.go:857-858` (≥ 0)
- state field: `dispatch.go:123`
- constructor wire: `dispatch.go:326-332`
- current use: `dispatch.go:763` — legacy PI path deadband only; energy-allocation path does **not** consult it
- units: W, site-signed

### Live grid meter read
- primary call: `dispatch.go:574-577` — `rawGridW := store.Get(state.SiteMeterDriver, telemetry.DerMeter).SmoothedW`
- same pattern in `applyFuseGuard`: `dispatch.go:1327-1332`
- same pattern in `forceFuseDischarge`: `dispatch.go:1775-1780`
- sign: site convention, positive = import
- smoothing: Kalman per (driver, type), process=100 W, meas=50 W

### Stale-meter guard
- location: `main.go:1383-1391`
- behaviour: `IsStale` → `SendDefault` for every driver and `continue`; `ComputeDispatch` is never entered when meter is stale
- implication: **the new clamp inside `ComputeDispatch` is automatically protected**

### Fuse guard
- aggregate: `applyFuseGuard` at `dispatch.go:1290-1338` (called `924`)
- reactive: `forceFuseDischarge` at `dispatch.go:1750+` (called `963`)
- `fuseMaxW` source: `main.go:1401` — `SiteFuseAmps * SiteFuseVoltage * SiteFusePhases`

### Slew
- `dispatch.go:876-894`; default 500 W/cycle (`Site.SlewRateW`); anchors on `SmoothedW`

### PI
- impl: `go/internal/control/pi.go:30-58`
- call site: `dispatch.go:778`
- defaults: `NewPI(Kp=0.5, Ki=0.1, IntegralLimit=3000, OutputLimit=10000)` at `dispatch.go:327`
- setpoint: `GridTargetW`

## Existing clamps review

| Clamp | Risk | Covers incident? |
|---|---|---|
| Watchdog (telemetry staleness) | silent driver / stale telemetry → SendDefault | no — different risk |
| Site-meter stale short-circuit | stale grid reading causing one battery to charge another | no — incident had fresh meter |
| Fuse guard (`applyFuseGuard`, bidirectional) | total predicted grid magnitude > fuse rating | no — 3 kW export << fuse (~11–25 kW) |
| Reactive fuse-saver (`forceFuseDischarge`) | live grid > fuse → force discharge | no — pushes discharge, doesn't limit it |
| Per-phase clamp | single phase > fuse_amps while aggregate under | no — phase geometry, not export |
| PI anti-windup (`IntegralLimit=3000`) | pinned actuator → integral runaway | no — bounds magnitude, not direction |
| Slew rate per driver | too-fast change → phase spike | partial — slows ramp, doesn't stop it |
| Per-command power cap (`MaxCommandW + DriverLimits`) | command > inverter cap | no — 5 kW is within cap |
| Battery cascade saturation curves | command exceeds empirical envelope per SoC | no — internal envelope |
| PV twin sanity envelopes | wild RLS coefficients | no — different twin |
| `applyPlanSignFloor` | dispatch sign ≠ plan slot intent → idle | **no — incident has matching signs**; bug is magnitude vs live meter |
| `planner_self` idle gate | tiny per-slot Wh allocation should pin to 0 | no — only fires in `planner_self` |
| Default mode on watchdog | EMS offline / driver excluded | no — fallback |

**Conclusion**: no existing clamp prevents "plan said discharge X, live
grid says you don't need that much, battery exports surplus". This is
a new, distinct quantifiable risk.

## Per-battery vs aggregate

- Plan target shape: single aggregate site-level number
  (`totalCorrection`, `dispatch.go:650`).
- Split point: `dispatch.go:850-858` — distribute splits across
  `onlineBats` after PI + corrections.
- **Apply the new clamp on the aggregate** (`totalCorrection` before
  distribute, or `sumTargets` after `applyPlanSignFloor`). Per-battery
  would over-clamp multi-battery sites.

## PV term

- Enters via `dispatch.go:1324-1326` inside `applyFuseGuard`.
- Site convention: `grid = load + bat + pv` (all signed). PV reduces
  import, so `rawGridW` already reflects PV.
- **No separate PV term needed** in the clamp.
- Formula sketch:

  ```text
  if want to charge more (target − current > 0):
      cap at max(0, -rawGridW - GridToleranceW)   # only export beyond deadband
  if want to discharge more (target − current < 0):
      cap at max(0, rawGridW - GridToleranceW)    # only import beyond deadband
  ```

## Saturation-curve feedback bug (avoid)

- Location: `go/internal/battery/model.go:339-357` (`updateCurve`).
- Guard: `MinSatSeedW = 1000` at `model.go:28`.
- Story: `docs/safety.md:355-372` — an earlier saturation-curve clamp
  recorded its own clamped output as the new envelope max for that SoC
  bucket → bucket locked at 255 W forever.
- Relevance: the new clamp must **not** feed its clamped output back
  as a PI measurement, saturation observation, or plan input.
- PI runs on `gridW` (live meter), not on the clamped command, so
  we're safe **if** the clamp acts after `state.PI.Update`.
- `PrevTargets` is updated post-clamp at `dispatch.go:1006`, but slew
  anchors on `SmoothedW` preferentially — the loop opens naturally.

## Existing tests

- Helper: `seedStore(gridW, []{name, currentW, soc})` at
  `control_test.go:54-72` — builds `telemetry.Store` with one meter
  and N batteries, marks all online via
  `DriverHealthMut().RecordSuccess()`.
- Patterns to mirror:
  - `control_test.go:117-148` — `SelfConsumption` discharge-on-import
    / charge-on-export — **perfect template**.
  - `control_test.go:281-368` — `FuseGuard` scaling tests — full-cycle
    `ComputeDispatch` with explicit fuse and meter.
  - `control_test.go:1830-1947` — `applyPlanSignFloor` tests.
- Test files:
  - `go/internal/control/control_test.go` (~85 KB, ~84 tests).
  - `go/internal/control/fuse_saver_test.go`
  - `go/internal/control/pv_curtail_test.go`

### Worker test recipe

**Over-prediction (the incident):**

```text
seedStore(+2000, [{"ferroamp", -5000, 0.5}])   # importing 2 kW, battery discharging -5 kW
mode = ModePlannerArbitrage with plan saying discharge
ComputeDispatch → assert sum(targets) >= rawGridW (no more than the
2 kW being imported, within ±GridToleranceW).
```

**Under-prediction (mirror):**

```text
seedStore(+1000, [{"ferroamp", 0, 0.5}])       # importing 1 kW, plan wants +5 kW charge
Plan / PI demands +5 kW; clamp at meter headroom → assert
sum(targets) <= rawGridW + GridToleranceW.
```

## Verification commands

- `cd go && go test ./internal/control/...` — fastest narrow loop (~5–15 s).
- `make test` — full unit+integration (30–90 s).
- `make e2e` — full-stack with sims, drivers, HTTP (~3 min).
- `cd go && go vet ./...` — static checks.

## Open questions for Judge

1. **Insertion point**: pre-distribute on `totalCorrection` (~line 780)
   vs. post-`applyPlanSignFloor` on `sumTargets` (~line 954). Scout
   leans pre-distribute (joint fuse allocator + distribute + slew +
   reclamp + fuseguard all assume aggregate).
2. **Existing deadband interaction**: replace or compose? Scout: compose
   — different shapes. Existing deadband skips dispatch entirely; new
   clamp bounds magnitude on cycles that survive.
3. **PI integral interaction**: persistent clamp activation will pin
   the integral at `IntegralLimit=3000` and unwind slowly when situation
   clears. Options: (a) accept, (b) reset PI on clamp fire, (c) feed
   clamped target back as anti-windup. Scout recommends (a) + log it.
4. **Per-battery vs aggregate**: aggregate before distribute.
5. **`manualHold` bypass?** Scout recommends **no** bypass (mirror
   `applyFuseGuard`); safety over convenience.
6. **Deadband choice**: owner said `GridToleranceW`. Follow brief
   (42 W default).
7. **`BatteryCoversEV` interaction**: use `rawGridW`, not the
   EV-subtracted `gridW`. The clamp's job is to prevent flow across the
   physical meter; EV draw shows up there regardless.
8. **Logging on activation**: yes, per goal constraint #6. Mirror
   `applyPlanSignFloor`'s `slog.Warn` at `dispatch.go:1621`.
