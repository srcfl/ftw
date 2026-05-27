# T002 Judge receipt — clamp design + Worker brief

## Decision

- **Insertion point**: A (pre-distribute) — insert at
  `dispatch.go ~line 781`, after `totalCorrection` is set by all four
  branches (manualHold / plannerSelfIdleGate / useEnergyPath / legacy
  PI), BEFORE the joint fuse allocator at line 809.
- **PI windup**: accept (option a). `IntegralLimit=3000` + slow unwind
  is the existing pattern.
- **`manualHold` bypass**: **no** — mirrors `applyFuseGuard`. Safety
  is non-optional even under operator override.
- **EV term**: use `rawGridW`, not `gridW − EV`.
- **`forceFuseDischarge` order**: unchanged — runs after the clamp so
  a real fuse overflow can still command extra discharge.

## Formula (Go pseudocode)

```go
// dispatch.go ~line 781 — after totalCorrection set by all four branches,
// before joint fuse allocator at ~809.

targetTotal := currentTotal + totalCorrection
dead := state.GridToleranceW
var allowed float64

switch {
case targetTotal > 0: // plan says charge
    headroom := -rawGridW - dead       // rawGridW < -dead ⇒ headroom > 0
    if headroom < 0 { headroom = 0 }
    if targetTotal > headroom {
        allowed = headroom              // never below 0; never flips sign
    } else {
        allowed = targetTotal
    }
case targetTotal < 0: // plan says discharge
    headroom := rawGridW - dead        // rawGridW > dead ⇒ headroom > 0
    if headroom < 0 { headroom = 0 }
    if -targetTotal > headroom {
        allowed = -headroom             // never above 0; never flips sign
    } else {
        allowed = targetTotal
    }
default:
    allowed = 0
}

if allowed != targetTotal {
    slog.Warn("dispatch: meter clamp reduced battery target",
        "requested_total_w", targetTotal,
        "clamped_total_w",   allowed,
        "raw_grid_w",        rawGridW,
        "current_total_w",   currentTotal,
        "deadband_w",        dead,
        "mode",              string(state.Mode))
    state.MeterClampActive = true
} else {
    state.MeterClampActive = false
}
totalCorrection = allowed - currentTotal
```

The clamp reduces magnitude only; it can drive to zero but it never
flips the sign of `totalCorrection`. Direction is the plan's
prerogative; magnitude is the meter's.

## Saturation feedback path

Safe. PI consumes `gridW` (live meter, dispatch.go:776-778), not the
clamped command, so the loop opens naturally. `PrevTargets` is set
post-clamp at dispatch.go:1006 but slew anchors on `SmoothedW`
preferentially. Saturation curves observe actual battery `SmoothedW`
with `MinSatSeedW=1000` guard. No residual feedback.

## Log fields

- `requested_total_w`
- `clamped_total_w`
- `raw_grid_w`
- `current_total_w`
- `deadband_w`
- `mode`

## Worker brief

- **Objective**: add an aggregate live-meter clamp on `totalCorrection`
  in `control.ComputeDispatch` (pre-distribute, post-PI), bounding
  battery charge to current export and battery discharge to current
  import, with `GridToleranceW` deadband; emit `slog.Warn` on
  activation; cover with two unit tests mirroring
  `control_test.go:117-148` `seedStore` pattern.

- **Allowed files**:
  - `go/internal/control/dispatch.go`
  - `go/internal/control/control_test.go`
  - `docs/safety.md` (only if there's a canonical clamp list to extend;
    otherwise skip)

- **Verify**:
  - `cd go && go test ./internal/control/... -run MeterClamp -count=1`
  - `cd go && go test ./internal/control/... -count=1`
  - `cd go && go vet ./...`
  - `make test`

- **Test names**:
  - `TestMeterClampStopsExportOnLoadOverPrediction`
  - `TestMeterClampStopsImportOnLoadUnderPrediction`

- **`stop_if`**:
  - existing tests in `go/internal/control/...` regress (FuseGuard,
    PlanSignFloor, SelfConsumption, FuseSaverBypassesSlew,
    JointFuseAllocatorWithBatteryCoversEV, PlannerSelfIdleGate*)
  - clamp changes sign of `totalCorrection`
  - clamp fires when `state.SiteMeterDriver` empty or meter reading
    absent (must noop, do not zero)
  - worker reaches for any file outside `allowed_files`
  - latency-hot path adds I/O, locks, or new goroutines
  - `applyPlanSignFloor` or `forceFuseDischarge` ordering modified
  - EV-subtracted `gridW` used instead of `rawGridW`
