package control

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
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

func TestFuseSaverRespectsExplicitZeroDischargeLimit(t *testing.T) {
	store, state, caps := setupFuseSaver(20000, 0, 0.6, 10000)
	state.DriverLimits["bat"] = PowerLimits{
		MaxChargeW:       10000,
		MaxDischargeWSet: true,
	}
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW != 0 || out[0].Clamped {
		t.Errorf("explicit zero discharge limit must close fuse-saver headroom: got %+v", out[0])
	}
}

func TestDeadbandFuseSaverRespectsExplicitZeroDischargeLimit(t *testing.T) {
	store, state, caps := setupFuseSaver(14000, 0, 0.6, 10000)
	state.DriverLimits["bat"] = PowerLimits{
		MaxChargeW:       10000,
		MaxDischargeWSet: true,
	}
	state.SetGridTarget(14000)
	state.MinDispatchIntervalS = 0

	if out := ComputeDispatch(store, state, caps, 11040); out != nil {
		t.Fatalf("deadband fuse saver crossed explicit zero discharge limit: %+v", out)
	}
}

func TestFuseSaverLegacyZeroWithoutSetFlagStillUsesDefault(t *testing.T) {
	store, state, caps := setupFuseSaver(20000, 0, 0.6, 0)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 0}}
	out := forceFuseDischarge(targets, store, state, caps, 11040)
	if out[0].TargetW != -MaxCommandW || !out[0].Clamped {
		t.Errorf("legacy zero without validity flag: got %+v, want -%d W clamped", out[0], MaxCommandW)
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
// DerReading.Data and a single battery on a 3Φ 230 V site. Aggregate
// gridW stays deliberately under the fuse to prove the per-phase clamp
// fires independently of the aggregate guard.
func setupPerPhase(aggGridW float64, l1A, l2A, l3A float64, batSoC float64, maxDischargeW float64) (*telemetry.Store, *State, map[string]float64) {
	return setupPerPhaseSite(3, 230, aggGridW, l1A, l2A, l3A, batSoC, maxDischargeW)
}

// setupPerPhaseSite is setupPerPhase with the site's fuse description
// spelled out. Phase count and per-phase voltage drive the conversion
// from a worst-phase overage into aggregate battery watts, so a test
// that cares about that conversion states them.
func setupPerPhaseSite(phases int, voltage float64, aggGridW float64, l1A, l2A, l3A float64, batSoC float64, maxDischargeW float64) (*telemetry.Store, *State, map[string]float64) {
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
	st.SiteFuseVoltage = voltage
	st.SiteFusePhases = phases
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
	// branch wouldn't fire. But the 4000 W charge adds 4000 / 690 =
	// 5.8 A to each phase, which takes L1 from 13 A to 18.8 A, over the
	// 15 A working limit (16 A fuse less 1 A) — only the per-phase
	// clamp can catch it.
	store, state, _ := setupPerPhase(7000, 13, 12, 8, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 4000}}
	guarded := applyFuseGuard(targets, store, state, 11040)
	if !guarded[0].Clamped {
		t.Errorf("Clamped flag must mark per-phase response")
	}
	// L1 has (15 − 13) × 690 = 1380 W of room for a balanced-3Φ
	// charge. Charge 4000 → 1380 W.
	expected := 1380.0
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
	out := fuseSaverFromLivePower(store, state, caps, 11040)
	if out == nil {
		t.Fatalf("per-phase overload from idle must trigger fuse-saver")
	}
	if out[0].TargetW >= 0 {
		t.Errorf("expected discharge target, got %.0f W", out[0].TargetW)
	}
	// L1 is (18 − 15) A over the working limit: 3 × 690 = 2070 W of
	// discharge from idle.
	expected := -2070.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("forced discharge = %.0f W, want %.0f W", out[0].TargetW, expected)
	}
}

// All phases under the fuse → per-phase clamp doesn't fire.
func TestPerPhaseClampNoOpWhenAllPhasesSafe(t *testing.T) {
	store, state, caps := setupPerPhase(8000, 14, 12, 10, 0.6, 10000)
	out := fuseSaverFromLivePower(store, state, caps, 11040)
	if out != nil {
		t.Errorf("all phases under fuse: expected nil, got %v", out)
	}
}

// Per-phase clamp gated on SiteFuseAmps > 0. Sites without per-phase
// configuration get the legacy aggregate-only behaviour.
func TestPerPhaseClampDisabledWhenSiteFuseAmpsZero(t *testing.T) {
	store, state, caps := setupPerPhase(9000, 18, 12, 8, 0.6, 10000)
	state.SiteFuseAmps = 0 // disable per-phase clamp
	out := fuseSaverFromLivePower(store, state, caps, 11040)
	if out != nil {
		t.Errorf("per-phase clamp disabled but still fired: %v", out)
	}
}

// Aggregate AND per-phase both over: take the larger overage. With a
// huge imbalance, per-phase × 3 dominates.
func TestPerPhaseClampDominatesAggregateWhenLarger(t *testing.T) {
	// Aggregate over by 500 W (predicted = 11540 vs 11040). But L1 is
	// at 22 A → over the 15 A working limit by 7 A × 230 = 1610 W
	// per-phase × 3 = 4830 W.
	store, state, caps := setupPerPhase(11540, 22, 14, 8, 0.6, 10000)
	out := fuseSaverFromLivePower(store, state, caps, 11040)
	if out == nil {
		t.Fatalf("expected discharge, got nil")
	}
	expected := -4830.0
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
	// Aggregate gridW = -7000 W (heavy export). Phases (magnitude, no
	// phase power, so they take the total's direction): L2 = 18 A (over
	// the 15 A working limit), L1 = 12, L3 = 8. Per-phase overage =
	// (18 − 15) × 230 × 3 = 2070 W of discharge to shrink. Battery is
	// currently discharging −5000 W; the target reduces magnitude to
	// bring L2 back under the limit.
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
	// New discharge magnitude: 5000 − 2070 = 2930 W → target = −2930 W.
	expected := -2930.0
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
	// Heavy export with one phase over fuse. fuseSaverFromLivePower (which
	// calls forceFuseDischarge) MUST return nil — the export-side
	// overage is not its problem.
	store, state, caps := setupPerPhase(-9000, 12, 18, 8, 0.6, 10000)
	out := fuseSaverFromLivePower(store, state, caps, 11040)
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
	// Phases (10, 7, 5). MaxAmps = 16, SafetyA = 2.0 → effective
	// threshold = 14 A, below the 1 A working margin's 15 A. The
	// 4000 W charge adds 5.8 A per phase, so L1 reaches 15.8 A: over
	// 14 A by 1.8 A, 1240 W of charge to shrink. Without the margin it
	// would be 550 W.
	store, state, _ := setupPerPhase(5000, 10, 7, 5, 0.6, 10000)
	state.SiteFuseSafetyA = 2.0
	state.SiteFusePhases = 3
	targets := []DispatchTarget{{Driver: "bat", TargetW: 4000}}
	guarded := applyFuseGuard(targets, store, state, 11040)
	if !guarded[0].Clamped {
		t.Errorf("safety margin should pull the threshold under L1")
	}
	// Aggregate effFuseW = 11040 − 2×230×3 = 9660. Predicted =
	// 5000 + 4000 − 0 = 9000 → no aggregate overage. Per-phase wins.
	// Buffer = half-margin = 0.5×2×230×3 = 690 W (post-fix headroom).
	// Charge 4000 − 1240 − 690 = 2070.
	expected := 2070.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("charge after safety-margin clamp = %.0f W, want %.0f W",
			guarded[0].TargetW, expected)
	}
}

// SafetyMarginA = 0 keeps the 1 A working margin the car uses (every
// existing per-phase test relies on it by leaving the field unset).
func TestPerPhaseClampSafetyMarginZeroIsBackCompat(t *testing.T) {
	store, state, _ := setupPerPhase(7000, 13, 12, 8, 0.6, 10000)
	state.SiteFuseSafetyA = 0
	state.SiteFusePhases = 3
	targets := []DispatchTarget{{Driver: "bat", TargetW: 4000}}
	guarded := applyFuseGuard(targets, store, state, 11040)
	// Same math as TestPerPhaseClampScalesChargingOnImbalance.
	expected := 1380.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("safety_margin_a=0 must keep the 1 A working margin: got %.0f W, want %.0f W",
			guarded[0].TargetW, expected)
	}
}

// ---- Per-phase relief is scaled by the site's phase count ----
//
// A worst-phase overage is watts on one phase; the dispatcher commands
// aggregate battery watts. The conversion between them is the site's
// phase count, and on a 1Φ site the aggregate meter and the single
// phase are the same wire — relief there is 1 × the overage, not 3 ×.
//
// A 1Φ site at 24 A on a 16 A fuse (1 A margin ⇒ 15 A threshold,
// 230 V): fuse budget 3680 W, aggregate ceiling 3450 W, meter at
// 5520 W. Worst-phase overage = (24 − 15) × 230 = 2070 W, which on
// this site is also exactly the aggregate overage — the two numbers
// must agree when there is only one phase.

// Import side, battery idle: the forced discharge equals the overage
// once, and leaves the meter on the import side of zero.
func TestPerPhaseReliefMatchesSinglePhaseSiteOnImport(t *testing.T) {
	store, state, caps := setupPerPhaseSite(1, 230, 5520, 24, 0, 0, 0.6, 10000)
	state.SiteFuseSafetyA = 1
	fuseMaxW := 16 * 230.0 * 1

	out := fuseSaverFromLivePower(store, state, caps, fuseMaxW)
	if out == nil {
		t.Fatalf("1Φ site at 24 A on a 16 A fuse: expected a forced discharge")
	}
	expected := -2070.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Fatalf("forced discharge = %.0f W, want %.0f W (overage × 1 phase, "+
			"not × 3)", out[0].TargetW, expected)
	}
	// The site has one phase, so the post-dispatch meter is that
	// phase. Demanding 3 × 2070 W would have driven 5520 W of import
	// to 690 W of export: relief the site never needed, pushing the
	// same breaker up the other way.
	if postGridW := 5520 + out[0].TargetW; postGridW < 0 {
		t.Errorf("post-dispatch grid = %.0f W: relief overshot through zero "+
			"into export", postGridW)
	}
}

// Export side — the direction forceFuseDischarge deliberately skips
// (more discharge would worsen it) and applyFuseGuard owns. The same
// phase-count law applies: shrink the discharge by the overage once.
func TestPerPhaseReliefMatchesSinglePhaseSiteOnExport(t *testing.T) {
	// Meter magnitudes mirror the import case: 5520 W out, 24 A on the
	// one phase. Pixii-style signed per-phase amps, so L1 reads −24.
	store, state, _ := setupPerPhaseSite(1, 230, -5520, -24, 0, 0, 0.6, 10000)
	state.SiteFuseSafetyA = 1
	fuseMaxW := 16 * 230.0 * 1

	// The battery is already discharging what the meter shows, and the
	// target holds it there — so the aggregate export overage is the
	// per-phase one. On a site with one phase the two must agree.
	setBatteryLiveW(store, "bat", -5000, 0.6)

	targets := []DispatchTarget{{Driver: "bat", TargetW: -5000}}
	guarded := applyFuseGuard(targets, store, state, fuseMaxW)
	if !guarded[0].Clamped {
		t.Fatalf("export-side per-phase overage must clamp the discharge target")
	}
	// Discharge 5000 − overage 2070 − buffer (half the 230 W margin)
	// = 2815 W. Demanding 2070 × 3 leaves nothing: the discharge is
	// zeroed and the site swings to 520 W of import.
	expected := -2815.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("discharge after 1Φ export clamp = %.1f W, want %.1f W",
			guarded[0].TargetW, expected)
	}
}

// setBatteryLiveW drives a battery's smoothed reading to w. The store
// Kalman-filters every sample, so one update lands about three quarters
// of the way there; repeat until the filter has converged.
func setBatteryLiveW(s *telemetry.Store, driver string, w, soc float64) {
	setBatteryLiveReading(s, driver, w, soc, nil)
}

func setBatteryLiveReading(s *telemetry.Store, driver string, w, soc float64, data json.RawMessage) {
	for i := 0; i < 12; i++ {
		s.Update(driver, telemetry.DerBattery, w, &soc, data)
	}
	s.DriverHealthMut(driver).RecordSuccess()
}

// A 1Φ site has no L2 and no L3. Current reported on a phase the site
// does not have is not a breaker this guard defends — reading it fires
// the clamp on a site that is nowhere near its fuse.
func TestPerPhaseClampReadsConfiguredPhasesOnly(t *testing.T) {
	store, state, caps := setupPerPhaseSite(1, 230, 2300, 10, 24, 24, 0.6, 10000)
	out := fuseSaverFromLivePower(store, state, caps, 16*230.0*1)
	if out != nil {
		t.Errorf("1Φ site with L1 at 10 A is under its fuse; l2_a/l3_a must "+
			"not fire the clamp. got %v", out)
	}
}

// Per-phase watts come from the configured voltage, the same source
// fuseSafetyMarginW uses. A 240 V site converts its amps at 240 V.
func TestPerPhaseClampUsesConfiguredVoltage(t *testing.T) {
	store, state, _ := setupPerPhaseSite(3, 240, 7000, 13, 12, 8, 0.6, 10000)
	targets := []DispatchTarget{{Driver: "bat", TargetW: 4000}}
	guarded := applyFuseGuard(targets, store, state, 16*240.0*3)
	if !guarded[0].Clamped {
		t.Fatalf("a 4000 W charge on L1 at 13 A on a 16 A fuse must clamp")
	}
	// L1 has (15 − 13) A of room below the working limit: 2 × 240 × 3
	// = 1440 W of charge. At the old hardcoded 230 V it would be 1380.
	expected := 1440.0
	if math.Abs(guarded[0].TargetW-expected) > 1 {
		t.Errorf("charge after per-phase clamp on a 240 V site = %.0f W, "+
			"want %.0f W", guarded[0].TargetW, expected)
	}
}

// Amps alone do not describe a fuse. Without a phase count and a
// voltage there is no conversion to watts, so the per-phase clamp
// stays off rather than inventing 230 V / 3Φ — the same law
// fuseSafetyMarginW follows. Production config always fills both
// (defaults 230 V / 3Φ, validation rejects <= 0).
func TestPerPhaseClampOffWhenFuseDescriptionIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phases  int
		voltage float64
	}{
		{"no phase count", 0, 230},
		{"no voltage", 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, state, caps := setupPerPhaseSite(
				tc.phases, tc.voltage, 9000, 18, 12, 8, 0.6, 10000)
			if out := fuseSaverFromLivePower(store, state, caps, 11040); out != nil {
				t.Errorf("incomplete fuse description must not clamp per phase, got %v", out)
			}
		})
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

// Joint EV/battery allocator caps EV draw against the peak ceiling in a
// planner mode where BatteryCoversEV=false (the EV throttles, the battery
// is not pulled in to cover steady state).
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
	st.Mode = ModePlannerSelf
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

// ---- The reactive deadband is not a safe state ----
//
// ComputeDispatch's legacy/reactive arm short-circuits when the grid
// error is inside GridToleranceW: nothing to correct, leave the
// batteries where they are. That exit used to return before
// applyFuseGuard, applyPlanSignFloor, applyBatteryBoostReserve and
// forceFuseDischarge — so on a site sitting inside the deadband no
// protection ran at all. The idle and holdoff exits already ran the
// fuse-saver for exactly this reason; this one was missed.
//
// The deadband asks one question: is aggregate grid close to aggregate
// target? Two protections answer to numbers that question never reads.

// An operator tariff ceiling below the plan's grid target. The plan
// asks for 8 kW of import, the meter delivers 8 kW, the error is zero
// — and every one of those watts above 5 kW is billed at the peak
// tariff the operator set the ceiling to avoid.
func TestDeadbandExitDefendsPeakImportCeiling(t *testing.T) {
	store, state, caps := setupFuseSaver(8000, 0, 0.6, 10000)
	state.SetGridTarget(8000) // plan target — meter is exactly on it
	state.PeakImportCeilingW = 5000
	state.MinDispatchIntervalS = 0

	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) == 0 {
		t.Fatalf("grid 8000 W against a 5000 W peak ceiling: expected the " +
			"fuse-saver to bridge, got no targets (3 kW peak overrun stands)")
	}
	// Predicted from zero = 8000. Effective import ceiling = 5000
	// (peak binds below the 11040 W fuse). Overage = 3000.
	expected := -3000.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("target = %.0f W, want %.0f W", out[0].TargetW, expected)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark fuse-saver activation")
	}
}

// An unbalanced site inside the aggregate deadband with one phase over
// the breaker: a single-phase EVSE on L1 against three-phase PV export.
// The aggregate says "nothing to do"; L1 is at 18 A on a 16 A fuse.
func TestDeadbandExitDefendsPerPhaseBreaker(t *testing.T) {
	store, state, caps := setupPerPhase(0, 18, 4, 4, 0.6, 10000)
	state.SetGridTarget(0) // aggregate is exactly on target
	state.MinDispatchIntervalS = 0

	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) == 0 {
		t.Fatalf("L1 at 18 A on a 16 A fuse inside the aggregate deadband: " +
			"expected a forced discharge, got no targets")
	}
	// Per-phase overage = (18 − 15) × 230 = 690 W over the working
	// limit on the worst phase, × 3 under the balanced-3Φ assumption
	// = 2070 W from idle.
	expected := -2070.0
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Errorf("forced discharge = %.0f W, want %.0f W", out[0].TargetW, expected)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark per-phase fuse-saver activation")
	}
}

// An early-exit fuse response must start from the battery's live power, not
// from idle. Replacing an existing discharge with a smaller discharge would
// increase import on every phase and make the overloaded phase worse.
func TestDeadbandExitPerPhaseReliefPreservesLiveDischarge(t *testing.T) {
	const (
		liveBatteryW = -5000.0
		liveWorstA   = 18.0
		fuseA        = 15.0 // 16 A fuse less the 1 A working margin
		phases       = 3.0
		voltage      = 230.0
	)
	store, state, caps := setupPerPhase(0, liveWorstA, 4, 4, 0.6, 10000)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.SetGridTarget(0)
	state.MinDispatchIntervalS = 0

	live := store.Get("bat", telemetry.DerBattery).SmoothedW
	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) != 1 {
		t.Fatalf("L1 at %.0f A with battery already discharging: got %+v, want one relief target", liveWorstA, out)
	}
	if out[0].TargetW > live {
		t.Fatalf("fuse relief reduced live discharge from %.0f W to %.0f W", live, out[0].TargetW)
	}

	// Under the dispatcher's balanced-3-phase conversion, the target delta
	// changes each phase by deltaW / (phases * voltage). The response must
	// not worsen the live worst phase and must relieve it to the fuse limit.
	postWorstA := liveWorstA + (out[0].TargetW-live)/(phases*voltage)
	if postWorstA > liveWorstA+0.01 {
		t.Fatalf("worst phase rose from %.2f A to %.2f A", liveWorstA, postWorstA)
	}
	if math.Abs(postWorstA-fuseA) > 0.01 {
		t.Errorf("worst phase after relief = %.2f A, want %.2f A", postWorstA, fuseA)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark per-phase fuse-saver activation")
	}
}

func TestDeadbandExitPerPhaseReliefStartsFromLiveCharge(t *testing.T) {
	const (
		liveBatteryW = 5000.0
		liveWorstA   = 18.0
		fuseA        = 15.0 // 16 A fuse less the 1 A working margin
		phases       = 3.0
		voltage      = 230.0
	)
	store, state, caps := setupPerPhase(0, liveWorstA, 4, 4, 0.6, 10000)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.SetGridTarget(0)
	state.MinDispatchIntervalS = 0

	live := store.Get("bat", telemetry.DerBattery).SmoothedW
	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) != 1 {
		t.Fatalf("L1 at %.0f A with battery charging: got %+v, want one relief target", liveWorstA, out)
	}
	expected := live - (liveWorstA-fuseA)*voltage*phases
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Fatalf("fuse relief target = %.0f W, want %.0f W from live charge %.0f W", out[0].TargetW, expected, live)
	}
	if out[0].TargetW <= 0 {
		t.Fatalf("relief of %.0f W needlessly reversed a %.0f W live charge", live-out[0].TargetW, live)
	}
	postWorstA := liveWorstA + (out[0].TargetW-live)/(phases*voltage)
	if math.Abs(postWorstA-fuseA) > 0.01 {
		t.Errorf("worst phase after relief = %.2f A, want %.2f A", postWorstA, fuseA)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark per-phase fuse-saver activation")
	}
}

func TestDeadbandExitCanCancelChargeBeforeUsingDischargeHeadroom(t *testing.T) {
	const (
		gridW        = 23000.0
		liveBatteryW = 5000.0
		fuseMaxW     = 11040.0
	)
	store, state, caps := setupFuseSaver(gridW, 0, 0.6, 10000)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0

	live := store.Get("bat", telemetry.DerBattery).SmoothedW
	out := ComputeDispatch(store, state, caps, fuseMaxW)
	if len(out) != 1 {
		t.Fatalf("grid %.0f W with battery charging: got %+v, want one relief target", gridW, out)
	}
	expected := live - (gridW - fuseMaxW)
	if math.Abs(out[0].TargetW-expected) > 1 {
		t.Fatalf("target = %.0f W, want %.0f W after cancelling charge and using discharge headroom", out[0].TargetW, expected)
	}
	if out[0].TargetW < -state.DriverLimits["bat"].MaxDischargeW {
		t.Fatalf("target %.0f W exceeds max discharge %.0f W", out[0].TargetW, state.DriverLimits["bat"].MaxDischargeW)
	}
	postGridW := gridW - live + out[0].TargetW
	if math.Abs(postGridW-fuseMaxW) > 1 {
		t.Errorf("post-dispatch grid = %.0f W, want fuse ceiling %.0f W", postGridW, fuseMaxW)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark fuse-saver activation")
	}
}

func TestDeadbandExitClampsLiveChargeBeforeFuseRelief(t *testing.T) {
	const (
		gridW        = 19040.0
		liveBatteryW = 10000.0
		maxChargeW   = 3000.0
		fuseMaxW     = 11040.0
	)
	store, state, caps := setupFuseSaver(gridW, 0, 0.6, 10000)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.DriverLimits["bat"] = PowerLimits{MaxChargeW: maxChargeW, MaxDischargeW: 10000}
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0

	live := store.Get("bat", telemetry.DerBattery).SmoothedW
	out := ComputeDispatch(store, state, caps, fuseMaxW)
	if len(out) != 1 {
		t.Fatalf("grid %.0f W with out-of-range live charge: got %+v, want one safe target", gridW, out)
	}
	if math.Abs(out[0].TargetW-2000) > 1 {
		t.Fatalf("target = %.0f W, want 2000 W after cap and fuse relief", out[0].TargetW)
	}
	if out[0].TargetW > maxChargeW {
		t.Fatalf("target %.0f W exceeds max charge %.0f W", out[0].TargetW, maxChargeW)
	}
	postGridW := gridW - live + out[0].TargetW
	if math.Abs(postGridW-fuseMaxW) > 1 {
		t.Errorf("post-dispatch grid = %.0f W, want fuse ceiling %.0f W", postGridW, fuseMaxW)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark cap and fuse relief")
	}
}

func TestDeadbandExitSelectsOnlyNeededOutOfCapChargeClamp(t *testing.T) {
	const (
		fuseMaxW   = 11040.0
		gridW      = fuseMaxW + 1000
		liveCharge = 10000.0
		maxCharge  = 3000.0
	)
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	setBatteryLiveW(store, "a", liveCharge, 0.6)
	setBatteryLiveW(store, "b", liveCharge, 0.6)
	state := NewState(0, 50, "meter")
	state.DriverLimits = map[string]PowerLimits{
		"a": {MaxChargeW: maxCharge, MaxDischargeW: 10000},
		"b": {MaxChargeW: maxCharge, MaxDischargeW: 10000},
	}
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0
	caps := map[string]float64{"b": 10000, "a": 10000}

	// Map iteration is deliberately unstable. Repeat the exact two-battery
	// case to prove that only one command-safe cap is selected each time.
	for i := 0; i < 100; i++ {
		out := ComputeDispatch(store, state, caps, fuseMaxW)
		if len(out) != 1 {
			t.Fatalf("run %d: got %+v, want one capped battery", i, out)
		}
		if out[0].Driver != "a" || math.Abs(out[0].TargetW-maxCharge) > 1 || !out[0].Clamped {
			t.Fatalf("run %d: got %+v, want deterministic a at safe cap %.0f W", i, out, maxCharge)
		}
		if out[0].TargetW > state.DriverLimits[out[0].Driver].MaxChargeW {
			t.Fatalf("run %d: target %.0f W exceeds max charge %.0f W", i, out[0].TargetW, maxCharge)
		}
		postGridW := gridW - liveCharge + out[0].TargetW
		if postGridW > fuseMaxW+1 || postGridW < -1 {
			t.Fatalf("run %d: post-dispatch grid %.0f W, want import within [0, %.0f] W", i, postGridW, fuseMaxW)
		}
	}
}

func TestDeadbandExitSkipsCapStepThatWouldBreakExportCeiling(t *testing.T) {
	const (
		fuseMaxW      = 11040.0
		gridW         = fuseMaxW + 1000
		liveChargeW   = 20000.0
		maxChargeW    = 3000.0
		maxExportW    = 1000.0
		capReliefW    = liveChargeW - maxChargeW
		maxSafeRelief = gridW + maxExportW
	)
	if capReliefW <= maxSafeRelief {
		t.Fatalf("test setup needs cap relief %.0f W above export budget %.0f W", capReliefW, maxSafeRelief)
	}

	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	setBatteryLiveW(store, "oversized", liveChargeW, 0.6)
	setBatteryLiveW(store, "safe", 0, 0.6)
	state := NewState(0, 50, "meter")
	state.MaxExportW = maxExportW
	state.DriverLimits = map[string]PowerLimits{
		"oversized": {MaxChargeW: maxChargeW, MaxDischargeW: 10000},
		"safe":      {MaxChargeW: 10000, MaxDischargeW: 10000},
	}
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0
	caps := map[string]float64{"oversized": 20000, "safe": 10000}

	out := ComputeDispatch(store, state, caps, fuseMaxW)
	if len(out) != 1 || out[0].Driver != "safe" {
		t.Fatalf("got %+v, want relief only from the in-range battery", out)
	}
	if math.Abs(out[0].TargetW-(-1000)) > 1 {
		t.Fatalf("safe target = %.0f W, want -1000 W", out[0].TargetW)
	}
	if postGridW := gridW + out[0].TargetW; math.Abs(postGridW-fuseMaxW) > 1 {
		t.Fatalf("post-dispatch grid = %.0f W, want %.0f W", postGridW, fuseMaxW)
	}
}

func TestDeadbandExitReturnsNoTargetWhenOnlyCapStepBreaksExportCeiling(t *testing.T) {
	const (
		fuseMaxW    = 11040.0
		gridW       = fuseMaxW + 1000
		liveChargeW = 20000.0
		maxChargeW  = 3000.0
	)
	store, state, caps := setupFuseSaver(gridW, 0, 0.6, 20000)
	setBatteryLiveW(store, "bat", liveChargeW, 0.6)
	state.MaxExportW = 1000
	state.DriverLimits["bat"] = PowerLimits{MaxChargeW: maxChargeW, MaxDischargeW: 10000}
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0

	if out := ComputeDispatch(store, state, caps, fuseMaxW); out != nil {
		t.Fatalf("unsafe discrete cap step must fail closed, got %+v", out)
	}
}

func TestDeadbandExitCapsPerPhaseReliefAtExportCeiling(t *testing.T) {
	const (
		gridW      = 1000.0
		maxExportW = 1000.0
		fuseMaxW   = 11040.0
	)
	store, state, caps := setupPerPhase(gridW, 20, 4, 4, 0.6, 10000)
	state.MaxExportW = maxExportW
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0

	out := ComputeDispatch(store, state, caps, fuseMaxW)
	if len(out) != 1 {
		t.Fatalf("got %+v, want one bounded phase-relief target", out)
	}
	if math.Abs(out[0].TargetW-(-2000)) > 1 {
		t.Fatalf("target = %.0f W, want -2000 W at the export ceiling", out[0].TargetW)
	}
	if postGridW := gridW + out[0].TargetW; math.Abs(postGridW-(-maxExportW)) > 1 {
		t.Fatalf("post-dispatch grid = %.0f W, want export ceiling %.0f W", postGridW, -maxExportW)
	}
}

func TestDeadbandExitFindsCapCombinationWithinExportBudget(t *testing.T) {
	const (
		gridW       = 9500.0
		importLimit = 500.0
		exportLimit = 500.0
		fuseMaxW    = 11040.0
		maxChargeW  = 1000.0
	)
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	setBatteryLiveW(store, "a", 5000, 0.02) // 4 kW discrete cap relief.
	setBatteryLiveW(store, "b", 7000, 0.02) // 6 kW discrete cap relief.
	setBatteryLiveW(store, "c", 8000, 0.02) // 7 kW discrete cap relief.
	state := NewState(0, 50, "meter")
	state.PeakImportCeilingW = importLimit
	state.MaxExportW = exportLimit
	state.DriverLimits = map[string]PowerLimits{
		"a": {MaxChargeW: maxChargeW, MaxDischargeW: 10000},
		"b": {MaxChargeW: maxChargeW, MaxDischargeW: 10000},
		"c": {MaxChargeW: maxChargeW, MaxDischargeW: 10000},
	}
	caps := map[string]float64{"a": 10000, "b": 10000, "c": 10000}

	out := fuseSaverFromLivePower(store, state, caps, fuseMaxW)
	if len(out) != 2 || out[0].Driver != "a" || out[1].Driver != "b" {
		t.Fatalf("got %+v, want the 4+6 kW covering subset", out)
	}
	for _, target := range out {
		if math.Abs(target.TargetW-maxChargeW) > 1 || !target.Clamped {
			t.Fatalf("unsafe cap target: %+v", target)
		}
	}
	if postGridW := gridW - 10000; math.Abs(postGridW-(-exportLimit)) > 1 {
		t.Fatalf("post-dispatch grid = %.0f W, want export ceiling %.0f W", postGridW, -exportLimit)
	}
}

func TestDeadbandExitChoosesCapWithEnoughContinuousHeadroom(t *testing.T) {
	const (
		gridW       = 5000.0
		reliefW     = 10000.0
		exportLimit = 5000.0
		fuseMaxW    = 11040.0
	)
	phaseData, _ := json.Marshal(map[string]any{
		"l1_a": 16 + reliefW/(3*230),
		"l2_a": 4.0,
		"l3_a": 4.0,
	})
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, gridW, nil, phaseData)
	store.DriverHealthMut("meter").RecordSuccess()
	setBatteryLiveW(store, "large-step", 9500, 0.02)
	setBatteryLiveW(store, "useful-room", 5000, 0.6)

	state := NewState(0, 50, "meter")
	state.SiteFuseAmps = 16
	state.SiteFuseVoltage = 230
	state.SiteFusePhases = 3
	state.MaxExportW = exportLimit
	state.DriverLimits = map[string]PowerLimits{
		"large-step":  {MaxChargeW: 500, MaxDischargeW: 10000},
		"useful-room": {MaxChargeW: 1000, MaxDischargeW: 10000},
	}
	caps := map[string]float64{"large-step": 10000, "useful-room": 10000}

	out := fuseSaverFromLivePower(store, state, caps, fuseMaxW)
	if len(out) != 1 || out[0].Driver != "useful-room" {
		t.Fatalf("got %+v, want the smaller cap with enough continuous headroom", out)
	}
	if math.Abs(out[0].TargetW-(-5000)) > 1 {
		t.Fatalf("target = %.0f W, want -5000 W at the export ceiling", out[0].TargetW)
	}
	if postGridW := gridW - 5000 + out[0].TargetW; math.Abs(postGridW-(-exportLimit)) > 1 {
		t.Fatalf("post-dispatch grid = %.0f W, want export ceiling %.0f W", postGridW, -exportLimit)
	}
}

func TestDeadbandExitCombinesCapsInLargeFleet(t *testing.T) {
	const (
		fuseMaxW = 11040.0
		gridW    = fuseMaxW + 3000
	)
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	state := NewState(0, 50, "meter")
	state.DriverLimits = make(map[string]PowerLimits)
	caps := make(map[string]float64)
	for i := 0; i < 13; i++ {
		name := fmt.Sprintf("bat-%02d", i)
		setBatteryLiveW(store, name, 2000, 0.02)
		state.DriverLimits[name] = PowerLimits{MaxChargeW: 1000, MaxDischargeW: 10000}
		caps[name] = 10000
	}

	out := fuseSaverFromLivePower(store, state, caps, fuseMaxW)
	if len(out) != 2 || out[0].Driver != "bat-00" || out[1].Driver != "bat-01" {
		t.Fatalf("got %+v, want the first two deterministic cap candidates", out)
	}
	for _, target := range out {
		if math.Abs(target.TargetW-500) > 1 || !target.Clamped {
			t.Fatalf("target = %+v, want +500 W after cap and shared relief", target)
		}
	}
	if postGridW := gridW - 4000 + 1000; math.Abs(postGridW-fuseMaxW) > 1 {
		t.Fatalf("post-dispatch grid = %.0f W, want fuse ceiling %.0f W", postGridW, fuseMaxW)
	}
}

func TestDeadbandExitEmitsCapClampWhenItAloneClearsFuseOverload(t *testing.T) {
	const (
		gridW        = 17040.0
		liveBatteryW = 10000.0
		maxChargeW   = 3000.0
		fuseMaxW     = 11040.0
	)
	store, state, caps := setupFuseSaver(gridW, 0, 0.6, 10000)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.DriverLimits["bat"] = PowerLimits{MaxChargeW: maxChargeW, MaxDischargeW: 10000}
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0

	live := store.Get("bat", telemetry.DerBattery).SmoothedW
	out := ComputeDispatch(store, state, caps, fuseMaxW)
	if len(out) != 1 {
		t.Fatalf("cap clamp that clears fuse overload returned %+v, want one target", out)
	}
	if math.Abs(out[0].TargetW-maxChargeW) > 1 {
		t.Fatalf("target = %.0f W, want max charge %.0f W", out[0].TargetW, maxChargeW)
	}
	if postGridW := gridW - live + out[0].TargetW; postGridW > fuseMaxW+1 {
		t.Errorf("post-dispatch grid = %.0f W, want at or below %.0f W", postGridW, fuseMaxW)
	}
	if !out[0].Clamped {
		t.Errorf("Clamped flag must mark the emitted cap clamp")
	}
}

func TestDeadbandExitDoesNotTakeControlForSafeOutOfCapLiveReading(t *testing.T) {
	const (
		gridW        = 5000.0
		liveBatteryW = 10000.0
		maxChargeW   = 3000.0
		fuseMaxW     = 11040.0
	)
	store, state, caps := setupFuseSaver(gridW, 0, 0.6, 10000)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.DriverLimits["bat"] = PowerLimits{MaxChargeW: maxChargeW, MaxDischargeW: 10000}
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0

	if out := ComputeDispatch(store, state, caps, fuseMaxW); out != nil {
		t.Fatalf("safe deadband took control of out-of-cap live reading: got %+v", out)
	}
	if state.LastDispatch != nil {
		t.Errorf("safe deadband recorded a dispatch")
	}
}

func TestDeadbandExitChargeCapCountsTowardPerPhaseRelief(t *testing.T) {
	const (
		liveBatteryW = 10000.0
		maxChargeW   = 3000.0
		liveWorstA   = 18.0
		fuseA        = 16.0
	)
	store, state, caps := setupPerPhase(0, liveWorstA, 4, 4, 0.6, 10000)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.DriverLimits["bat"] = PowerLimits{MaxChargeW: maxChargeW, MaxDischargeW: 10000}
	state.SetGridTarget(0)
	state.MinDispatchIntervalS = 0

	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) != 1 {
		t.Fatalf("got %+v, want one capped target", out)
	}
	if math.Abs(out[0].TargetW-maxChargeW) > 1 {
		t.Fatalf("target = %.0f W, want cap %.0f W without duplicate phase relief", out[0].TargetW, maxChargeW)
	}
	postWorstA := liveWorstA + (out[0].TargetW-liveBatteryW)/(3*230)
	if postWorstA > fuseA+0.01 {
		t.Errorf("worst phase after cap = %.2f A, want at or below %.2f A", postWorstA, fuseA)
	}
}

func TestDeadbandExitUsesOnlyRemainingPerPhaseReliefAfterChargeCap(t *testing.T) {
	const (
		liveBatteryW = 5000.0
		maxChargeW   = 4500.0
		liveWorstA   = 18.0
		fuseA        = 15.0 // 16 A fuse less the 1 A working margin
		phaseReliefW = (liveWorstA - fuseA) * 230 * 3
	)
	store, state, caps := setupPerPhase(0, liveWorstA, 4, 4, 0.6, 10000)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.DriverLimits["bat"] = PowerLimits{MaxChargeW: maxChargeW, MaxDischargeW: 10000}
	state.SetGridTarget(0)
	state.MinDispatchIntervalS = 0

	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) != 1 {
		t.Fatalf("got %+v, want one capped relief target", out)
	}
	want := maxChargeW - (phaseReliefW - (liveBatteryW - maxChargeW))
	if math.Abs(out[0].TargetW-want) > 1 {
		t.Fatalf("target = %.0f W, want %.0f W after counting the cap delta once", out[0].TargetW, want)
	}
	postWorstA := liveWorstA + (out[0].TargetW-liveBatteryW)/(3*230)
	if math.Abs(postWorstA-fuseA) > 0.01 {
		t.Errorf("worst phase after relief = %.2f A, want %.2f A", postWorstA, fuseA)
	}
}

func TestDeadbandExitChargeBlockCountsTowardPerPhaseRelief(t *testing.T) {
	const (
		liveBatteryW = 5000.0
		liveWorstA   = 18.0
	)
	store, state, caps := setupPerPhase(0, liveWorstA, 4, 4, 0.6, 10000)
	setBatteryLiveReading(store, "bat", liveBatteryW, 0.6,
		json.RawMessage(`{"discharge_capable":true,"charge_capable":false}`))

	out := fuseSaverFromLivePower(store, state, caps, 11040)
	if len(out) != 1 {
		t.Fatalf("got %+v, want one charge-stop target", out)
	}
	if math.Abs(out[0].TargetW) > 1 {
		t.Fatalf("charge block target = %.0f W, want 0 W without duplicate phase relief", out[0].TargetW)
	}
}

func TestDeadbandExitEmptyBatteryOnlyCancelsLiveCharge(t *testing.T) {
	for _, tc := range []struct {
		name       string
		overageW   float64
		wantTarget float64
		wantGridW  float64
	}{
		{name: "partial cancellation", overageW: 3000, wantTarget: 2000, wantGridW: 11040},
		{name: "residual overload", overageW: 7000, wantTarget: 0, wantGridW: 13040},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const (
				liveBatteryW = 5000.0
				fuseMaxW     = 11040.0
			)
			gridW := fuseMaxW + tc.overageW
			store, state, caps := setupFuseSaver(gridW, 0, 0.02, 10000)
			setBatteryLiveW(store, "bat", liveBatteryW, 0.02)
			state.SetGridTarget(gridW)
			state.MinDispatchIntervalS = 0

			live := store.Get("bat", telemetry.DerBattery).SmoothedW
			out := ComputeDispatch(store, state, caps, fuseMaxW)
			if len(out) != 1 {
				t.Fatalf("empty battery live charge: got %+v, want one cancellation target", out)
			}
			if math.Abs(out[0].TargetW-tc.wantTarget) > 1 {
				t.Fatalf("target = %.0f W, want %.0f W", out[0].TargetW, tc.wantTarget)
			}
			if out[0].TargetW < 0 {
				t.Fatalf("empty battery was asked to discharge at %.0f W", out[0].TargetW)
			}
			postGridW := gridW - live + out[0].TargetW
			if math.Abs(postGridW-tc.wantGridW) > 1 {
				t.Errorf("post-dispatch grid = %.0f W, want %.0f W", postGridW, tc.wantGridW)
			}
		})
	}
}

func TestDeadbandExitBlockedDischargeOnlyCancelsLiveCharge(t *testing.T) {
	for _, tc := range []struct {
		name         string
		liveW        float64
		overageW     float64
		wantTarget   float64
		wantDispatch bool
	}{
		{name: "partial cancellation", liveW: 5000, overageW: 3000, wantTarget: 2000, wantDispatch: true},
		{name: "residual overload", liveW: 5000, overageW: 7000, wantTarget: 0, wantDispatch: true},
		{name: "live discharge is not weakened", liveW: -1000, overageW: 1000, wantDispatch: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const fuseMaxW = 11040.0
			gridW := fuseMaxW + tc.overageW
			store, state, caps := setupFuseSaver(gridW, 0, 0.6, 10000)
			setBatteryLiveReading(store, "bat", tc.liveW, 0.6,
				json.RawMessage(`{"discharge_capable":false,"charge_capable":true}`))
			state.SetGridTarget(gridW)
			state.MinDispatchIntervalS = 0

			out := ComputeDispatch(store, state, caps, fuseMaxW)
			if !tc.wantDispatch {
				if out != nil {
					t.Fatalf("blocked live discharge got a weaker fuse command: %+v", out)
				}
				if state.LastDispatch != nil {
					t.Errorf("blocked live discharge recorded a dispatch")
				}
				return
			}
			if len(out) != 1 {
				t.Fatalf("blocked-discharge battery: got %+v, want one safe target", out)
			}
			if math.Abs(out[0].TargetW-tc.wantTarget) > 1 {
				t.Fatalf("target = %.0f W, want %.0f W", out[0].TargetW, tc.wantTarget)
			}
			if out[0].TargetW < 0 {
				t.Fatalf("blocked-discharge battery was asked for %.0f W", out[0].TargetW)
			}
		})
	}
}

func TestDeadbandExitDoesNotWeakenOutOfRangeLiveDischarge(t *testing.T) {
	const (
		gridW         = 12000.0
		liveBatteryW  = -10000.0
		maxDischargeW = 3000.0
		fuseMaxW      = 11040.0
	)
	store, state, caps := setupFuseSaver(gridW, 0, 0.6, maxDischargeW)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0

	if out := ComputeDispatch(store, state, caps, fuseMaxW); out != nil {
		t.Fatalf("out-of-range live discharge got a weaker fuse command: %+v", out)
	}
	if state.LastDispatch != nil {
		t.Errorf("out-of-range live discharge recorded a dispatch")
	}
	live := store.Get("bat", telemetry.DerBattery).SmoothedW
	postClampGridW := gridW - live - maxDischargeW
	if math.Abs(postClampGridW-19000) > 1 {
		t.Fatalf("test setup no longer proves that clamping to -%.0f W would raise grid to 19000 W; got %.0f W", maxDischargeW, postClampGridW)
	}
}

func TestDeadbandExitPerPhaseDoesNotWeakenOutOfRangeLiveDischarge(t *testing.T) {
	const (
		liveBatteryW  = -10000.0
		maxDischargeW = 3000.0
		liveWorstA    = 18.0
	)
	store, state, caps := setupPerPhase(0, liveWorstA, 4, 4, 0.6, maxDischargeW)
	setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
	state.SetGridTarget(0)
	state.MinDispatchIntervalS = 0

	if out := ComputeDispatch(store, state, caps, 11040); out != nil {
		t.Fatalf("per-phase fuse path weakened live discharge: %+v", out)
	}
	worstIfClampedA := liveWorstA + (-maxDischargeW-liveBatteryW)/(3*230)
	if worstIfClampedA < 28 {
		t.Fatalf("test setup no longer proves the unsafe phase increase: %.2f A", worstIfClampedA)
	}
}

func TestDeadbandExitRoutesReliefAroundUnsafeLiveDischarge(t *testing.T) {
	const (
		gridW        = 14000.0
		fuseMaxW     = 11040.0
		blockedLiveW = -1000.0
	)
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, gridW, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	setBatteryLiveReading(store, "blocked", blockedLiveW, 0.6,
		json.RawMessage(`{"discharge_capable":false,"charge_capable":true}`))
	setBatteryLiveW(store, "capable", 0, 0.6)
	state := NewState(0, 50, "meter")
	state.DriverLimits = map[string]PowerLimits{
		"blocked": {MaxChargeW: 5000, MaxDischargeW: 5000},
		"capable": {MaxChargeW: 5000, MaxDischargeW: 5000},
	}
	state.SetGridTarget(gridW)
	state.MinDispatchIntervalS = 0
	caps := map[string]float64{"blocked": 10000, "capable": 10000}

	out := ComputeDispatch(store, state, caps, fuseMaxW)
	if len(out) != 1 {
		t.Fatalf("got %+v, want only the capable sibling", out)
	}
	if out[0].Driver != "capable" {
		t.Fatalf("unsafe live-discharge driver was commanded: %+v", out)
	}
	if math.Abs(out[0].TargetW-(-2960)) > 1 {
		t.Fatalf("capable sibling target = %.0f W, want -2960 W", out[0].TargetW)
	}
	capableLive := store.Get("capable", telemetry.DerBattery).SmoothedW
	if postGridW := gridW - capableLive + out[0].TargetW; math.Abs(postGridW-fuseMaxW) > 1 {
		t.Errorf("post-dispatch grid = %.0f W, want %.0f W", postGridW, fuseMaxW)
	}
}

func TestEarlyFuseSnapshotFailsClosedOnTelemetryOrLimitDrift(t *testing.T) {
	t.Run("battery sample", func(t *testing.T) {
		store, state, caps := setupFuseSaver(12000, -5000, 0.6, 10000)
		meter := store.Get("meter", telemetry.DerMeter)
		_, safe, versions := fleetAtLivePower(store, caps, state.DriverLimits)
		if !fuseSnapshotStillCurrent(store, state, meter.UpdatedAt, versions, safe) {
			t.Fatalf("unchanged snapshot was rejected")
		}
		setBatteryLiveW(store, "bat", 0, 0.6)
		if fuseSnapshotStillCurrent(store, state, meter.UpdatedAt, versions, safe) {
			t.Fatalf("battery drift was accepted")
		}
	})

	t.Run("meter sample", func(t *testing.T) {
		store, state, caps := setupFuseSaver(12000, 0, 0.6, 10000)
		meter := store.Get("meter", telemetry.DerMeter)
		_, safe, versions := fleetAtLivePower(store, caps, state.DriverLimits)
		store.Update("meter", telemetry.DerMeter, 13000, nil, nil)
		if fuseSnapshotStillCurrent(store, state, meter.UpdatedAt, versions, safe) {
			t.Fatalf("meter drift was accepted")
		}
	})

	t.Run("capability sample", func(t *testing.T) {
		store, state, caps := setupFuseSaver(14040, 0, 0.6, 10000)
		setBatteryLiveReading(store, "bat", 5000, 0.6,
			json.RawMessage(`{"discharge_capable":true,"charge_capable":true}`))
		meter := store.Get("meter", telemetry.DerMeter)
		_, safe, versions := fleetAtLivePower(store, caps, state.DriverLimits)
		setBatteryLiveReading(store, "bat", 5000, 0.6,
			json.RawMessage(`{"discharge_capable":true,"charge_capable":false}`))
		if fuseSnapshotStillCurrent(store, state, meter.UpdatedAt, versions, safe) {
			t.Fatalf("capability drift was accepted")
		}
	})

	t.Run("command limit", func(t *testing.T) {
		store, state, caps := setupFuseSaver(14040, 0, 0.6, 10000)
		setBatteryLiveW(store, "bat", 5000, 0.6)
		meter := store.Get("meter", telemetry.DerMeter)
		_, safe, versions := fleetAtLivePower(store, caps, state.DriverLimits)
		state.DriverLimits["bat"] = PowerLimits{MaxChargeW: 1000, MaxDischargeW: 10000}
		if fuseSnapshotStillCurrent(store, state, meter.UpdatedAt, versions, safe) {
			t.Fatalf("target outside a changed command limit was accepted")
		}
	})
}

func TestFuseSaverMixedFleetHonorsPerBatteryLowerBounds(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("meter", telemetry.DerMeter, 20000, nil, nil)
	store.DriverHealthMut("meter").RecordSuccess()
	setBatteryLiveReading(store, "blocked", 3000, 0.6,
		json.RawMessage(`{"discharge_capable":false,"charge_capable":true}`))
	setBatteryLiveW(store, "capable", -1000, 0.6)
	state := NewState(0, 50, "meter")
	state.DriverLimits = map[string]PowerLimits{
		"blocked": {MaxChargeW: 5000, MaxDischargeW: 5000},
		"capable": {MaxChargeW: 5000, MaxDischargeW: 5000},
	}
	targets := []DispatchTarget{
		{Driver: "blocked", TargetW: 3000},
		{Driver: "capable", TargetW: -1000},
	}
	out := forceFuseDischarge(targets, store, state,
		map[string]float64{"blocked": 10000, "capable": 10000}, 11040)
	if len(out) != 2 {
		t.Fatalf("got %d targets, want 2", len(out))
	}
	if out[0].TargetW < -0.001 {
		t.Fatalf("blocked battery crossed below zero: %.0f W", out[0].TargetW)
	}
	if out[1].TargetW < -5000.001 {
		t.Fatalf("capable battery crossed discharge cap: %.0f W", out[1].TargetW)
	}
	if math.Abs(out[0].TargetW-0) > 1 || math.Abs(out[1].TargetW-(-5000)) > 1 {
		t.Errorf("targets = [%.0f, %.0f] W, want [0, -5000] W", out[0].TargetW, out[1].TargetW)
	}
}

func TestDeadbandExitWithLiveBatteryPowerStaysQuietWhenNothingBinds(t *testing.T) {
	for _, liveBatteryW := range []float64{-3000, 3000} {
		t.Run(fmt.Sprintf("live_%+.0f_w", liveBatteryW), func(t *testing.T) {
			store, state, caps := setupFuseSaver(2000, 0, 0.6, 10000)
			setBatteryLiveW(store, "bat", liveBatteryW, 0.6)
			state.SetGridTarget(2000)
			state.MinDispatchIntervalS = 0

			if out := ComputeDispatch(store, state, caps, 11040); out != nil {
				t.Fatalf("safe deadband with live battery %.0f W: got %+v, want no dispatch", liveBatteryW, out)
			}
			if state.LastDispatch != nil {
				t.Errorf("safe deadband with live battery %.0f W recorded a dispatch", liveBatteryW)
			}
		})
	}
}

// The deadband still means "do nothing" when nothing binds. Running
// the fuse-saver on this exit must not turn a quiet tick into a
// dispatch: no targets, and no LastDispatch stamp that would start the
// holdoff clock against a cycle that commanded nothing.
func TestDeadbandExitStaysQuietWhenNothingBinds(t *testing.T) {
	store, state, caps := setupFuseSaver(2000, 0, 0.6, 10000)
	state.SetGridTarget(2000)
	state.MinDispatchIntervalS = 0

	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) != 0 {
		t.Fatalf("grid on target, fuse and phases clear: expected no targets, got %+v", out)
	}
	if state.LastDispatch != nil {
		t.Errorf("a no-op deadband tick must not record a dispatch")
	}
}

// The fuse-saver fires from the deadband exit even while the holdoff
// would otherwise suppress a re-dispatch on the following tick: firing
// records the dispatch, so the next tick is the holdoff's to decide —
// and the holdoff exit runs the same fuse-saver anyway.
func TestDeadbandExitRecordsDispatchWhenFuseSaverFires(t *testing.T) {
	store, state, caps := setupFuseSaver(8000, 0, 0.6, 10000)
	state.SetGridTarget(8000)
	state.PeakImportCeilingW = 5000
	state.MinDispatchIntervalS = 5

	out := ComputeDispatch(store, state, caps, 11040)
	if len(out) == 0 {
		t.Fatalf("expected fuse-saver discharge")
	}
	if state.LastDispatch == nil {
		t.Errorf("a firing fuse-saver must record the dispatch")
	}
	if len(state.LastTargets) != len(out) {
		t.Errorf("LastTargets = %+v, want the issued targets %+v", state.LastTargets, out)
	}
}
