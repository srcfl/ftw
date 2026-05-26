package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// Reactive fuse-saver tests. These guard the contract: under no
// software-controllable circumstance should the operator's hardware
// fuse trip because the EMS sat idle while the meter went over the
// limit. The PR description has the full background — surfaced by
// the manual_hold ramp test where the EV was pinned at high amps
// while the home battery was idle per the planner's slot.

func setupFuseSaver(gridW, batW float64, batSoC float64, maxDischargeW float64) (*telemetry.Store, *State, map[string]float64) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	soc := batSoC
	s.Update("bat", telemetry.DerBattery, batW, &soc, nil)
	s.DriverHealthMut("bat").RecordSuccess()
	st := NewState(0, 50, "meter")
	st.DriverLimits = map[string]PowerLimits{
		"bat": {MaxChargeW: 10000, MaxDischargeW: maxDischargeW},
	}
	return s, st, map[string]float64{"bat": 15200}
}

// Idle battery + grid surge over fuse → battery is forced to discharge
// to bring grid back under the fuse. This is the manual_hold-ramp
// scenario from the PR description.
func TestFuseSaverForcesDischargeFromIdle(t *testing.T) {
	// Grid importing 14 kW (e.g. EV pinned at 11 kW + house 3 kW),
	// fuse 11.04 kW. Battery currently idle.
	store, state, caps := setupFuseSaver(14000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if len(out) != 1 {
		t.Fatalf("expected 1 target, got %d", len(out))
	}
	// Predicted = 14000 - 0 + 0 = 14000. Overage = 14000 - 11040 = 2960.
	// Battery has 10 kW of discharge headroom → absorb full overage.
	expected := -2960.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f, want %.0f (full overage absorbed by idle battery)",
			out[0].TargetW, expected)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark fuse-saver activation")
	}
}

// In production the call chain is applyFuseGuard THEN
// forceFuseDischarge. When the planner asks for charge while the grid
// is already at the fuse, applyFuseGuard scales the charge down toward
// 0; forceFuseDischarge then sees a small (or zero) target and either
// no-ops or adds a tiny extra discharge to close the residual gap.
// The standalone-flip-to-discharge behaviour the previous version of
// this test asserted doesn't happen on the real path — applyFuseGuard
// would never let a +3 kW charge reach forceFuseDischarge with grid
// already at the fuse.
func TestFuseSaverAfterFuseGuardKeepsScaledCharge(t *testing.T) {
	// Live grid near the fuse limit; planner asked to charge 3 kW;
	// battery currently idle.
	store, state, caps := setupFuseSaver(11000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 3000}}

	// Step 1: applyFuseGuard scales charging down because predicted
	// import (11000 + 3000 = 14000) exceeds the fuse.
	guarded := applyFuseGuard(targets, store, state, 11040)
	if guarded[0].TargetW < 0 {
		t.Fatalf("applyFuseGuard should NOT flip charge to discharge — "+
			"got %.0f W", guarded[0].TargetW)
	}
	if guarded[0].TargetW > 100 {
		t.Fatalf("applyFuseGuard should have scaled charge down toward 0, "+
			"got %.0f W (charge surviving the fuse guard is a regression)",
			guarded[0].TargetW)
	}

	// Step 2: forceFuseDischarge runs on the post-guard targets. With
	// charge already at ~0 the predicted gridW after the guard is at
	// the fuse limit; forceFuseDischarge no-ops or adds a small
	// residual discharge. Either way it must NOT take the target
	// further negative than -2960 W (the original overage).
	out := forceFuseDischarge(guarded, store, state, caps, 11040)
	if out[0].TargetW < -2960.001 {
		t.Errorf("post-guard discharge over-correction: target = %.0f W, "+
			"original overage was 2960 W", out[0].TargetW)
	}
}

// Battery already commanded to discharge a bit; fuse-saver TOPS UP the
// discharge instead of starting from scratch.
func TestFuseSaverAddsToExistingDischarge(t *testing.T) {
	store, state, caps := setupFuseSaver(13000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: -1000}}
	// Predicted = 13000 - 0 + (-1000) = 12000. Over fuse by 960.
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	expected := -1960.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f, want %.0f (existing discharge plus overage)",
			out[0].TargetW, expected)
	}
}

// Empty pack (SoC < 5%) → can't be drained. Function returns the
// targets unchanged. Hardware fuse is the next layer.
func TestFuseSaverRespectsEmptyBattery(t *testing.T) {
	store, state, caps := setupFuseSaver(14000, 0, 0.02, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW != 0 || out[0].Clamped {
		t.Errorf("empty battery should not be drained: got %.0f clamped=%v",
			out[0].TargetW, out[0].Clamped)
	}
}

// MaxDischargeW caps the fuse-saver — never command more discharge
// than the battery can physically deliver, even if the fuse would
// otherwise be violated by even more.
func TestFuseSaverRespectsMaxDischarge(t *testing.T) {
	// Massive overage (10 kW), but battery can only discharge 4 kW.
	store, state, caps := setupFuseSaver(20000, 0, 0.6, 4000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW < -4000.001 {
		t.Errorf("target = %.0f, exceeds MaxDischargeW=4000",
			out[0].TargetW)
	}
}

// Within fuse → no-op. The fuse-saver doesn't touch dispatch when
// predicted gridW is already safe.
func TestFuseSaverNoOpWhenWithinFuse(t *testing.T) {
	store, state, caps := setupFuseSaver(5000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 1000}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW != 1000 || out[0].Clamped {
		t.Errorf("within fuse: got %.0f clamped=%v, want 1000 false",
			out[0].TargetW, out[0].Clamped)
	}
}

// Multi-battery: distribute the forced discharge proportionally to
// each battery's remaining headroom.
func TestFuseSaverDistributesAcrossBatteries(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, 14000, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	soc := 0.6
	s.Update("a", telemetry.DerBattery, 0, &soc, nil)
	s.DriverHealthMut("a").RecordSuccess()
	s.Update("b", telemetry.DerBattery, 0, &soc, nil)
	s.DriverHealthMut("b").RecordSuccess()
	state := NewState(0, 50, "meter")
	state.DriverLimits = map[string]PowerLimits{
		"a": {MaxChargeW: 10000, MaxDischargeW: 6000},
		"b": {MaxChargeW: 10000, MaxDischargeW: 4000},
	}
	caps := map[string]float64{"a": 10000, "b": 5000}
	targets := []DispatchTarget{
		{Driver: "a", TargetW: 0},
		{Driver: "b", TargetW: 0},
	}
	// Overage 14000 - 11040 = 2960. Distributed by headroom (6:4):
	// a gets 2960*0.6 = 1776, b gets 2960*0.4 = 1184.
	out := forceFuseDischarge(targets, s, state, caps, 11040)
	if math.Abs(out[0].TargetW-(-1776)) > 1 {
		t.Errorf("battery a: %.0f, want -1776", out[0].TargetW)
	}
	if math.Abs(out[1].TargetW-(-1184)) > 1 {
		t.Errorf("battery b: %.0f, want -1184", out[1].TargetW)
	}
	var sum float64
	for _, t := range out {
		sum += t.TargetW
	}
	if math.Abs(sum-(-2960)) > 1 {
		t.Errorf("total forced discharge = %.0f, want -2960", sum)
	}
}

// fuseMaxW=0 → disabled. No-op.
func TestFuseSaverDisabledWhenFuseMaxZero(t *testing.T) {
	store, state, caps := setupFuseSaver(20000, 0, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 1000}}
	out := forceFuseDischarge(targets, store, state, caps, 0)
	if out[0].TargetW != 1000 || out[0].Clamped {
		t.Errorf("fuse_max=0 should disable: got %.0f clamped=%v",
			out[0].TargetW, out[0].Clamped)
	}
}

// Slew bypass — the fuse is the non-negotiable ceiling, slew rate
// must NOT limit the fuse-saver. End-to-end test: battery at rest
// (anchor SmoothedW = 0), aggressive 500 W/cycle slew, sudden grid
// import 14 kW (over 11 kW fuse). Expected: target ≈ -3 kW
// regardless of the slew rate, because forceFuseDischarge runs
// AFTER the slew loop in ComputeDispatch.
func TestFuseSaverBypassesSlew(t *testing.T) {
	store, state, caps := setupFuseSaver(14000, 0, 0.6, 10000)
	state.Mode = ModeIdle
	state.SlewRateW = 500 // tight slew that would otherwise cap the response
	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) == 0 {
		t.Fatalf("idle + over-fuse: expected fuse-saver discharge, got empty")
	}
	// Predicted overage = 14000 − 11040 = 2960 W. With 10 kW headroom
	// the fuse-saver should command the full overage as discharge —
	// well beyond the 500 W/cycle slew. If we see −500 W instead of
	// ≈ −2960 W, slew is incorrectly clamping the safety primary.
	expected := -2960.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f W, want %.0f W (slew must NOT clamp the fuse-saver)",
			out[0].TargetW, expected)
	}
}

// ---- Per-phase clamp ----

// setupPerPhase wires a meter that emits l1_a/l2_a/l3_a in
// DerReading.Data and a single battery. Aggregate gridW stays
// deliberately under the fuse to prove the per-phase clamp fires
// independently of the aggregate guard.
func setupPerPhase(aggGridW float64, l1A, l2A, l3A float64, batSoC float64, maxDischargeW float64) (*telemetry.Store, *State, map[string]float64) {
	s := telemetry.NewStore()
	data, _ := json.Marshal(map[string]any{
		"l1_a": l1A,
		"l2_a": l2A,
		"l3_a": l3A,
	})
	s.Update("meter", telemetry.DerMeter, aggGridW, nil, data)
	s.DriverHealthMut("meter").RecordSuccess()
	soc := batSoC
	s.Update("bat", telemetry.DerBattery, 0, &soc, nil)
	s.DriverHealthMut("bat").RecordSuccess()
	st := NewState(0, 50, "meter")
	st.SiteFuseAmps = 16
	st.SiteFuseVoltage = 230
	st.DriverLimits = map[string]PowerLimits{
		"bat": {MaxChargeW: 10000, MaxDischargeW: maxDischargeW},
	}
	return s, st, map[string]float64{"bat": 15200}
}

// Single-phase imbalance: aggregate is under fuse but L1 is over.
// applyFuseGuard must scale charging down to bring the worst phase
// back. (Pixii single-phase battery on L1 is the real-world case.)
func TestPerPhaseClampScalesChargingOnImbalance(t *testing.T) {
	// Isolate the per-phase case: aggregate predicted (7000 + 4000 =
	// 11000 W) is under the 11040 W fuse, so the legacy aggregate
	// branch wouldn't fire. But L1 alone is at 18 A × 230 V = 4140 W
	// (over the 16 A fuse on that phase) — only the per-phase clamp
	// can catch it.
	store, state, _ := setupPerPhase(7000, 18, 12, 8, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 4000}}
	guarded := applyFuseGuard(targets, store, state, 11040)
	if !guarded[0].Clamped {
		t.Errorf("Clamped flag must mark per-phase response")
	}
	// Per-phase overage = (18 − 16) × 230 = 460 W on the worst phase.
	// Balanced-3Φ assumption ⇒ 460 × 3 = 1380 W of total battery
	// reduction needed. Charge 4000 → 4000 − 1380 = 2620 W.
	expected := 2620.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("charge after per-phase clamp = %.0f W, want %.0f W",
			guarded[0].TargetW, expected)
	}
}

// Per-phase overage with no charge to scale → fuse-saver forces
// discharge. Reproduces the test-day failure: battery idle, EV pinned
// at 16 A 3Φ, one phase pushed over by house imbalance, aggregate
// stays under fuse so applyFuseGuard wouldn't fire — but the per-phase
// path now does.
func TestPerPhaseClampFiresFuseSaverFromIdle(t *testing.T) {
	store, state, caps := setupPerPhase(9000, 18, 12, 8, 0.6, 10000)
	out := fuseSaverFromZero(store, state, caps, 11040)
	if out == nil {
		t.Fatalf("per-phase overload from idle must trigger fuse-saver")
	}
	if out[0].TargetW >= 0 {
		t.Errorf("expected discharge target, got %.0f W", out[0].TargetW)
	}
	// Same math as the charging test: 460 W × 3 = 1380 W. From idle
	// (target 0), full discharge of 1380 W.
	expected := -1380.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("forced discharge = %.0f W, want %.0f W", out[0].TargetW, expected)
	}
}

// All phases under the fuse → per-phase clamp doesn't fire.
func TestPerPhaseClampNoOpWhenAllPhasesSafe(t *testing.T) {
	store, state, caps := setupPerPhase(8000, 14, 12, 10, 0.6, 10000)
	out := fuseSaverFromZero(store, state, caps, 11040)
	if out != nil {
		t.Errorf("all phases under fuse: expected nil, got %v", out)
	}
}

// Per-phase clamp gated on SiteFuseAmps > 0. Sites without per-phase
// configuration get the legacy aggregate-only behaviour.
func TestPerPhaseClampDisabledWhenSiteFuseAmpsZero(t *testing.T) {
	store, state, caps := setupPerPhase(9000, 18, 12, 8, 0.6, 10000)
	state.SiteFuseAmps = 0 // disable per-phase clamp
	out := fuseSaverFromZero(store, state, caps, 11040)
	if out != nil {
		t.Errorf("per-phase clamp disabled but still fired: %v", out)
	}
}

// Aggregate AND per-phase both over: take the larger overage. With a
// huge imbalance, per-phase × 3 dominates.
func TestPerPhaseClampDominatesAggregateWhenLarger(t *testing.T) {
	// Aggregate over by 500 W (predicted = 11540 vs 11040). But L1 is
	// at 22 A → over by 6 A × 230 = 1380 W per-phase × 3 = 4140 W.
	store, state, caps := setupPerPhase(11540, 22, 14, 8, 0.6, 10000)
	out := fuseSaverFromZero(store, state, caps, 11040)
	if out == nil {
		t.Fatalf("expected discharge, got nil")
	}
	expected := -4140.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("forced discharge = %.0f W, want %.0f W (per-phase × 3)",
			out[0].TargetW, expected)
	}
}

// Per-phase EXPORT overage: PV is exporting hard on a single phase
// (e.g. 1Φ inverter on L2), aggregate gridW negative. The per-phase
// guard must fire on the EXPORT side and shrink discharging — NOT
// trigger forceFuseDischarge, which would push more power out and
// worsen the over-current phase. Regression for PR #219 review S1.
func TestPerPhaseClampFiresOnExportImbalance(t *testing.T) {
	// Aggregate gridW = -7000 W (heavy export). Phases (magnitude):
	// L2 = 18 A (over the 16 A fuse), L1 = 12, L3 = 8. Per-phase
	// overage = (18 − 16) × 230 × 3 = 1380 W of discharge to shrink.
	// Battery is currently discharging −5000 W; the target reduces
	// magnitude to bring L2 back under fuse.
	store, state, _ := setupPerPhase(-7000, 12, 18, 8, 0.6, 10000)
	// Set the live battery reading so applyFuseGuard's "predicted"
	// reflects the current discharge state.
	soc := 0.6
	store.Update("bat", telemetry.DerBattery, -5000, &soc, nil)
	store.DriverHealthMut("bat").RecordSuccess()

	targets := []DispatchTarget{{Driver: "bat", TargetW: -5000}}
	guarded := applyFuseGuard(targets, store, state, 11040)
	if !guarded[0].Clamped {
		t.Errorf("export-side per-phase overage must clamp the discharge target")
	}
	// New discharge magnitude: 5000 − 1380 = 3620 W → target = −3620 W.
	expected := -3620.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("discharge after export-side per-phase clamp = %.0f W, want %.0f W",
			guarded[0].TargetW, expected)
	}
}

func TestFuseGuardPredictsAgainstControlledBatteriesOnly(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, -8000, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()

	soc := 0.6
	store.Update("online", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("online").RecordSuccess()

	// Stale/offline battery readings can still exist in the store and
	// are already included in the live meter reading. applyFuseGuard must
	// not subtract them unless it is also replacing them with a target.
	store.Update("offline", telemetry.DerBattery, -3000, &soc, nil)
	store.DriverHealthMut("offline").SetOffline()

	state := NewState(0, 50, "meter")
	targets := []DispatchTarget{{Driver: "online", TargetW: -3000}}
	guarded := applyFuseGuard(targets, store, state, 10000)
	if !guarded[0].Clamped {
		t.Fatalf("expected export-side fuse guard to clamp controlled discharge")
	}
	// True post-dispatch grid is -8000 - current_online(0) + target(-3000)
	// = -11000 W, so the 10 kW export fuse is exceeded by 1000 W.
	expected := -2000.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("controlled discharge after fuse guard = %.0f W, want %.0f W",
			guarded[0].TargetW, expected)
	}
}

// forceFuseDischarge must NOT fire on export-side per-phase overage.
// Its job is to relieve IMPORT — commanding more discharge during
// export-side per-phase trip would push the over-current phase
// further over the breaker. Regression for PR #219 review S1.
func TestForceFuseDischargeIgnoresExportSidePerPhase(t *testing.T) {
	// Heavy export with one phase over fuse. fuseSaverFromZero (which
	// calls forceFuseDischarge) MUST return nil — the export-side
	// overage is not its problem.
	store, state, caps := setupPerPhase(-9000, 12, 18, 8, 0.6, 10000)
	out := fuseSaverFromZero(store, state, caps, 11040)
	if out != nil {
		t.Errorf("forceFuseDischarge fired on export-side per-phase overage; "+
			"would worsen the over-current phase. got %v", out)
	}
}

// SiteFuseSafetyA shrinks the per-phase threshold below MaxAmps so the
// dispatch stops trying to ride right up to the breaker (where the
// inverter's own per-phase limiter trips first and causes a flap).
// Regression for PR #219 — the bug class motivating the safety margin.
func TestPerPhaseClampHonorsSafetyMargin(t *testing.T) {
	// Phases (15.5, 12, 8). MaxAmps = 16, SafetyA = 1.0 → effective
	// threshold = 15 A. L1 at 15.5 A is OVER the effective threshold
	// (under the raw 16 A). Per-phase overage = (15.5 − 15) × 230 × 3
	// = 345 W of charge to shrink.
	store, state, _ := setupPerPhase(7000, 15.5, 12, 8, 0.6, 10000)
	state.SiteFuseSafetyA = 1.0
	state.SiteFusePhases = 3
	targets := []DispatchTarget{{Driver: "bat", TargetW: 4000}}
	guarded := applyFuseGuard(targets, store, state, 11040)
	if !guarded[0].Clamped {
		t.Errorf("safety margin should pull the threshold under L1 = 15.5 A")
	}
	// Aggregate effFuseW = 11040 − 1.0×230×3 = 10350. Predicted =
	// 7000 + 4000 − 0 = 11000 → aggregate overage = 11000 − 10350 = 650.
	// Per-phase overage × 3 = 345. Aggregate wins (650 > 345).
	// Buffer = half-margin = 0.5×1×230×3 = 345 W (post-fix headroom).
	// Charge 4000 − 650 − 345 = 3005.
	expected := 3005.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("charge after safety-margin clamp = %.0f W, want %.0f W",
			guarded[0].TargetW, expected)
	}
}

// SafetyMarginA = 0 must produce identical output to the pre-PR
// behaviour (locks the back-compat contract every existing per-phase
// test relies on by leaving the field unset).
func TestPerPhaseClampSafetyMarginZeroIsBackCompat(t *testing.T) {
	store, state, _ := setupPerPhase(7000, 18, 12, 8, 0.6, 10000)
	state.SiteFuseSafetyA = 0
	state.SiteFusePhases = 3
	targets := []DispatchTarget{{Driver: "bat", TargetW: 4000}}
	guarded := applyFuseGuard(targets, store, state, 11040)
	// Same math as TestPerPhaseClampScalesChargingOnImbalance.
	expected := 2620.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("safety_margin_a=0 must match pre-PR behaviour: got %.0f W, want %.0f W",
			guarded[0].TargetW, expected)
	}
}

// End-to-end via ComputeDispatch: idle mode + grid surge → returns
// non-empty discharge targets. Idle mode would normally return [].
func TestFuseSaverFiresInIdleMode(t *testing.T) {
	store, state, caps := setupFuseSaver(14000, 0, 0.6, 10000)
	state.Mode = ModeIdle
	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) == 0 {
		t.Fatalf("idle mode + over-fuse import: expected fuse-saver discharge, got empty")
	}
	if out[0].TargetW >= 0 {
		t.Errorf("expected discharge target, got %.0f", out[0].TargetW)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark fuse-saver activation")
	}
}

// Operator-report 2026-04-30: with safety_margin_a=1 (threshold = 15 A)
// the system stabilised with phase amps oscillating between 15.0 and
// 15.8 A — fuse guard reactively scaled targets just enough to clear
// the trip, then the planner re-ramped, then it re-tripped. Half-margin
// buffer keeps post-clamp aggregate below the threshold by an explicit
// margin so the next tick has room before re-tripping.
func TestFuseGuardLeavesHeadroomBelowThreshold(t *testing.T) {
	store, state, _ := setupPerPhase(-7000, 12, 18, 8, 0.6, 10000)
	state.SiteFuseSafetyA = 1.0
	state.SiteFusePhases = 3
	soc := 0.6
	store.Update("bat", telemetry.DerBattery, -5000, &soc, nil)
	store.DriverHealthMut("bat").RecordSuccess()

	out := applyFuseGuard(
		[]DispatchTarget{{Driver: "bat", TargetW: -5000}},
		store, state, 11040)
	if !out[0].Clamped {
		t.Fatalf("expected per-phase export clamp")
	}
	// perPhase = (18−15) × 230 × 3 = 2070 W
	// half-margin buffer = 0.5 × 1A × 230V × 3p = 345 W
	// totalDischarge = 5000 → newTotal = 5000 − 2070 − 345 = 2585
	expected := -2585.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("post-clamp target = %.0f W, want %.0f W (overage + half-margin)",
			out[0].TargetW, expected)
	}
}

// Hold-mode hysteresis prevents the planner from immediately
// re-ramping into the threshold on the next tick. The clamp's max
// is latched for ~30 s and re-applied even when the live overage
// is zero.
func TestFuseGuardHoldsClampAcrossTicks(t *testing.T) {
	// Tick 1: clamp fires, latches max.
	store, state, _ := setupPerPhase(-7000, 12, 18, 8, 0.6, 10000)
	state.SiteFuseSafetyA = 1.0
	state.SiteFusePhases = 3
	soc := 0.6
	store.Update("bat", telemetry.DerBattery, -5000, &soc, nil)
	store.DriverHealthMut("bat").RecordSuccess()

	tick1 := applyFuseGuard(
		[]DispatchTarget{{Driver: "bat", TargetW: -5000}},
		store, state, 11040)
	if !tick1[0].Clamped {
		t.Fatalf("tick 1: expected initial clamp")
	}
	if state.FuseHoldMaxDischargeW <= 0 {
		t.Fatalf("tick 1: hold-max should be latched, got %v", state.FuseHoldMaxDischargeW)
	}
	if state.FuseHoldUntil.IsZero() {
		t.Fatalf("tick 1: hold-until should be set")
	}
	tick1Cap := state.FuseHoldMaxDischargeW

	// Tick 2: phase amps look clean (battery responded), planner
	// asks for the original full discharge again. Hold-mode must
	// re-apply the latched cap.
	store2, state2, _ := setupPerPhase(-3000, 8, 10, 6, 0.6, 10000)
	state2.SiteFuseSafetyA = 1.0
	state2.SiteFusePhases = 3
	state2.FuseHoldMaxDischargeW = state.FuseHoldMaxDischargeW
	state2.FuseHoldUntil = state.FuseHoldUntil
	store2.Update("bat", telemetry.DerBattery, -2500, &soc, nil)
	store2.DriverHealthMut("bat").RecordSuccess()

	tick2 := applyFuseGuard(
		[]DispatchTarget{{Driver: "bat", TargetW: -5000}},
		store2, state2, 11040)
	if !tick2[0].Clamped {
		t.Errorf("tick 2: hold-mode should still clamp the re-ramp")
	}
	if -tick2[0].TargetW > tick1Cap+0.5 {
		t.Errorf("tick 2: target magnitude %.0f exceeds latched cap %.0f — hold not enforced",
			-tick2[0].TargetW, tick1Cap)
	}
}

// Hold expires after the window so future planner re-ramps aren't
// permanently capped by stale state.
func TestFuseGuardHoldExpiresAfterWindow(t *testing.T) {
	store, state, _ := setupPerPhase(-3000, 8, 10, 6, 0.6, 10000)
	state.SiteFuseSafetyA = 1.0
	state.SiteFusePhases = 3
	soc := 0.6
	store.Update("bat", telemetry.DerBattery, -2500, &soc, nil)
	store.DriverHealthMut("bat").RecordSuccess()

	state.FuseHoldMaxDischargeW = 1000
	state.FuseHoldUntil = time.Now().Add(-1 * time.Second)

	out := applyFuseGuard(
		[]DispatchTarget{{Driver: "bat", TargetW: -4000}},
		store, state, 11040)
	if !state.FuseHoldUntil.IsZero() {
		t.Errorf("expired hold-until should be cleared, got %v", state.FuseHoldUntil)
	}
	if state.FuseHoldMaxDischargeW != 0 {
		t.Errorf("expired hold-max should be cleared, got %v", state.FuseHoldMaxDischargeW)
	}
	if out[0].Clamped {
		t.Errorf("post-expiry, no live overage: target should pass through, got %v", out[0])
	}
}

// ---- PeakImportCeilingW (tariff peak, hard rule across modes) ----

// Default 0 → no behaviour change. Same scenario as
// TestFuseSaverNoOpWhenWithinFuse but explicit about the field default.
func TestPeakCeilingDisabledByDefaultIsNoop(t *testing.T) {
	store, state, caps := setupFuseSaver(8000, 0, 0.6, 10000)
	if state.PeakImportCeilingW != 0 {
		t.Fatalf("PeakImportCeilingW must default to 0 (disabled), got %v", state.PeakImportCeilingW)
	}
	out := forceFuseDischarge(
		[]DispatchTarget{{Driver: "bat", TargetW: 0}},
		store, state, caps, 11040)
	if out[0].TargetW != 0 || out[0].Clamped {
		t.Errorf("default-disabled peak: target should pass through, got %v", out[0])
	}
}

// forceFuseDischarge fires when grid exceeds the operator's tariff peak
// even though the fuse is intact. Battery briefly bridges; the joint
// allocator has already throttled the EV via FuseEVMaxW.
func TestPeakCeilingForcesDischargeBelowFuse(t *testing.T) {
	// Grid at 8 kW import, well under the 11.04 kW fuse but over a
	// 5 kW tariff peak.
	store, state, caps := setupFuseSaver(8000, 0, 0.6, 10000)
	state.PeakImportCeilingW = 5000
	out := forceFuseDischarge(
		[]DispatchTarget{{Driver: "bat", TargetW: 0}},
		store, state, caps, 11040)
	// Predicted = 8000. Effective import ceiling = 5000. Overage = 3000.
	expected := -3000.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f, want %.0f (peak overrun fully covered by battery)",
			out[0].TargetW, expected)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark peak-saver activation")
	}
}

// Joint EV/battery allocator caps EV draw against the peak ceiling
// across modes — the operator's tariff is honoured even with
// BatteryCoversEV=false (the EV throttles, the battery isn't pulled
// in to cover steady-state).
func TestPeakCeilingClampsEVViaJointAllocator(t *testing.T) {
	// House load 1 kW, EV pinned at 7 kW = 8 kW import. Peak 5 kW.
	// Fuse 11 kW (won't fire). Battery idle (no charge or discharge).
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, 8000, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	soc := 0.6
	store.Update("bat", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("bat").RecordSuccess()
	st := NewState(0, 50, "meter")
	st.PeakImportCeilingW = 5000
	st.EVChargingW = 7000
	st.BatteryCoversEV = false
	st.DriverLimits = map[string]PowerLimits{"bat": {MaxChargeW: 5000, MaxDischargeW: 10000}}
	caps := map[string]float64{"bat": 15200}

	// Run a full dispatch cycle so the joint allocator's geometry
	// fires (it lives inside ComputeDispatch, not as a free function).
	st.Mode = ModeSelfConsumption
	_ = ComputeDispatch(store, st, caps, 11040)

	// Joint allocator should have set FuseEVMaxW so that
	// H + Bn + B*scale + E*scale ≤ 5000 (peak ceiling).
	// H = rawGrid − currentBat − E = 8000 − 0 − 7000 = 1000.
	// Bn = 0 (idle), B may be small from PI but the cap should land
	// near (5000 − 1000) = 4000 W of EV draw.
	if !st.FuseSaturated {
		t.Errorf("FuseSaturated must be true under peak overrun, got false")
	}
	// EV cap must drop well below the uncapped 7000 W. Exact value
	// depends on PI internals (a small steady-state discharge command
	// is legitimately accounted for by the joint allocator), so the
	// assertion is "capped meaningfully", not an exact number. Anything
	// above ~5500 means the peak ceiling isn't biting.
	if st.FuseEVMaxW == 0 {
		t.Errorf("FuseEVMaxW = 0 — peak ceiling should permit *some* EV draw")
	}
	if st.FuseEVMaxW > 5500 {
		t.Errorf("FuseEVMaxW = %.0f, want ≤ 5500 (peak 5000 binding cap)", st.FuseEVMaxW)
	}
}

// Peak above fuse is a no-op: fuse is the binding ceiling.
// Sanity check the helper picks the tighter of the two.
func TestPeakCeilingAboveFuseFusewins(t *testing.T) {
	store, state, caps := setupFuseSaver(12000, 0, 0.6, 10000)
	state.PeakImportCeilingW = 50000 // operator typo / loose tariff
	out := forceFuseDischarge(
		[]DispatchTarget{{Driver: "bat", TargetW: 0}},
		store, state, caps, 11040)
	// effFuseW = 11040, peak ignored (looser). Overage = 12000 − 11040 = 960.
	expected := -960.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f, want %.0f (fuse should bind when peak is looser)",
			out[0].TargetW, expected)
	}
}
