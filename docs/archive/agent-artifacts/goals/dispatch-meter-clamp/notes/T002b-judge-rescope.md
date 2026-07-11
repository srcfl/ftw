# T002b Judge receipt — clamp re-scoped to legacy PI branch

## Decision

**Scoped path.** The clamp lives inside the `default:` arm of the
`totalCorrection` switch (`dispatch.go:739-780`), immediately after
`totalCorrection = out.Output` at line 779. Structural placement
scopes it; no extra boolean guard is needed because `manualHold`,
`plannerSelfIdleGate`, and `useEnergyPath` each take their own arm and
never reach `default:`.

## Why not universal

The three other branches encode deliberate, tested contracts:

- **`manualHold`** is operator override. The setpoint executes
  exactly (`TestBatteryManualHoldChargesAtSetpoint` etc.). CLAUDE.md:
  *"manual hold ... skips ... the deadband"*.
- **`useEnergyPath`** is planner_arbitrage / planner_cheap. Per
  `go/internal/control/CLAUDE.md`: *"the whole point of these modes
  is to cycle the battery across the zero-grid line on purpose"* —
  Wh per slot is the optimisation variable; meter-clamping defeats
  arbitrage.
- **`plannerSelfIdleGate`** must drive to 0 regardless of grid.

The original incident (load-twin over-prediction, reactive PI
over-discharges with no meter check) sits inside `default:`. Fix
matches the actual failure mode; nothing else changes.

## Insertion

```
go/internal/control/dispatch.go:779
```

Immediately after `totalCorrection = out.Output` and before the closing
brace of the `default:` branch at line 780. Variables in scope:
`rawGridW`, `currentTotal`, `state.GridToleranceW`, `effectiveMode`.

## PM amendment (post-T003b discovery)

Worker found that subtracting `dead` from the headroom double-counts
the deadband: it already gates entry at `dispatch.go:763` (PI skips
when `|errW| < tol`). Re-subtracting inside the clamp leaves a
deadband-width gap that breaks the `TestSlewRespectsRateWhenTracking`
contract (rawGridW=+1500, expects discharge to exactly -1500).

Fix: deadband is a **threshold** (don't fire below it), not a
**haircut** (don't subtract from the headroom).

```go
case targetTotal < 0:                  // plan says discharge
    headroom := rawGridW                // not (rawGridW - dead)
    if headroom < dead { headroom = 0 } // threshold, not subtraction
    ...
case targetTotal > 0:                  // plan says charge
    headroom := -rawGridW
    if headroom < dead { headroom = 0 }
    ...
```

Updated formula below reflects this.

## Formula (Go)

```go
// Inside the default arm, after `totalCorrection = out.Output`:
targetTotal := currentTotal + totalCorrection
dead := state.GridToleranceW
var allowed float64
switch {
case targetTotal > 0: // plan says charge → cap at current export
    headroom := -rawGridW
    if headroom < dead {
        headroom = 0   // threshold, not haircut
    }
    if targetTotal > headroom {
        allowed = headroom
    } else {
        allowed = targetTotal
    }
case targetTotal < 0: // plan says discharge → cap at current import
    headroom := rawGridW
    if headroom < dead {
        headroom = 0   // threshold, not haircut
    }
    if -targetTotal > headroom {
        allowed = -headroom
    } else {
        allowed = targetTotal
    }
default:
    allowed = 0
}
if allowed != targetTotal {
    slog.Warn("dispatch: meter clamp reduced battery target",
        "requested_total_w", targetTotal,
        "clamped_total_w", allowed,
        "raw_grid_w", rawGridW,
        "current_total_w", currentTotal,
        "deadband_w", dead,
        "mode", string(effectiveMode))
}
totalCorrection = allowed - currentTotal
```

## Test recipes

### `TestMeterClampStopsExportOnLoadOverPrediction`

```text
seedStore(+2000, [{"ferroamp", -5000, 0.5}])
st := NewState(0, 42, "ferroamp")
st.Mode = ModeSelfConsumption
st.SlewRateW = 100000  // slew won't mask the clamp
Expect: sum(targets) >= rawGridW - GridToleranceW
        (no more discharge than current import; clamp only reduces magnitude)
```

### `TestMeterClampStopsImportOnLoadUnderPrediction`

```text
seedStore(+1000, [{"ferroamp", 0, 0.5}])
st := NewState(-5000, 42, "ferroamp")   // negative GridTargetW → PI demands large charge
st.Mode = ModeSelfConsumption
st.SlewRateW = 100000
Expect: sum(targets) <= max(0, -rawGridW - GridToleranceW) ≈ 0
        (charge clamped near zero because meter is importing, not exporting)
```

Both tests use `ModeSelfConsumption` → legacy PI default branch →
clamp fires. None of the regressing tests exercise that branch with
the failing geometry, so zero regressions expected.

## Worker brief

- **Objective**: add the meter clamp on `totalCorrection` inside the
  legacy PI `default:` branch (`dispatch.go:779`), bounding charge to
  current export and discharge to current import via `rawGridW` and
  `state.GridToleranceW`; `slog.Warn` on activation; two unit tests
  against `ModeSelfConsumption`.
- **Allowed files**:
  - `go/internal/control/dispatch.go`
  - `go/internal/control/control_test.go`
- **Verify**:
  - `cd go && go test ./internal/control/... -run MeterClamp -count=1`
  - `cd go && go test ./internal/control/... -count=1`
  - `cd go && go vet ./...`
  - `make test`
- **`stop_if`**: any test regression in `go/internal/control/...`;
  clamp placed outside `default:`; sign of `totalCorrection` flipped;
  `gridW` used instead of `rawGridW`; `applyPlanSignFloor` or
  `forceFuseDischarge` reordered; I/O / locks / goroutines on hot
  path; files outside `allowed_files`.
- Expected zero regressions.

## Required board updates

- `T002` stays done (the prior brief is superseded, not invalidated;
  insertion-point and formula are reused, only the branch placement
  changes).
- `T002b` becomes done with this receipt.
- `T003` → re-activate with the refined brief (or spawn `T003b`).
- After Worker lands: add a line to `go/internal/control/CLAUDE.md`'s
  "What NOT to do" section: *"Do NOT hoist the meter clamp out of the
  legacy `default:` branch — manualHold, plannerSelfIdleGate, and
  useEnergyPath have their own contracts."* (Out of slice for now;
  follow-up.)
