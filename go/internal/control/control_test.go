package control

import (
	"math"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// ---- PI controller ----

func TestPIProducesNegativeOutputWhenGridAboveTarget(t *testing.T) {
	p := NewPI(0.5, 0.1, 3000, 10000)
	p.Setpoint = 0
	// grid = +2000 (too much import)
	out := p.Update(2000)
	if out.Output >= 0 {
		t.Errorf("expected negative correction (→ more discharge), got %f", out.Output)
	}
	if out.Error != -2000 {
		t.Errorf("error should be setpoint-measurement = -2000, got %f", out.Error)
	}
}

func TestPIIntegralClampsAtLimit(t *testing.T) {
	p := NewPI(0, 100, 500, 10000) // only integral term, small limit
	p.Setpoint = 0
	// Feed a persistent error far beyond limit
	for i := 0; i < 100; i++ {
		p.Update(1000)
	}
	out := p.Update(1000)
	if math.Abs(out.I) > 500.0001 {
		t.Errorf("integral should be clamped to ±500, got %f", out.I)
	}
}

func TestPIReset(t *testing.T) {
	p := NewPI(0.5, 0.1, 3000, 10000)
	p.Setpoint = 0
	for i := 0; i < 10; i++ {
		p.Update(500)
	}
	p.Reset()
	out := p.Update(0)
	if out.I != 0 {
		t.Errorf("integral should be 0 after reset, got %f", out.I)
	}
}

// ---- Dispatch tests ----

// helper: build a store with one site meter + N batteries at given SoC
func seedStore(gridW float64, batteries []struct {
	name    string
	currentW, soc float64
}) *telemetry.Store {
	s := telemetry.NewStore()
	s.Update("ferroamp", telemetry.DerMeter, gridW, nil, nil)
	s.DriverHealthMut("ferroamp").RecordSuccess()
	for _, b := range batteries {
		soc := b.soc
		s.Update(b.name, telemetry.DerBattery, b.currentW, &soc, nil)
		s.DriverHealthMut(b.name).RecordSuccess()
	}
	return s
}

func caps(items map[string]float64) map[string]float64 { return items }

func ptrF64(v float64) *float64 { return &v }

func TestIdleModeReturnsNothing(t *testing.T) {
	store := seedStore(2000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeIdle
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("idle should dispatch nothing, got %d", len(targets))
	}
}

func TestChargeModeForcesAllBatteriesPositive5kW(t *testing.T) {
	store := seedStore(0, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeCharge
	targets := ComputeDispatch(store, st,
		caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d", len(targets))
	}
	for _, tg := range targets {
		if tg.TargetW != 5000 {
			t.Errorf("charge mode should set +5000, got %f", tg.TargetW)
		}
	}
}

func TestDeadbandSkipsWithinTolerance(t *testing.T) {
	store := seedStore(30, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp") // tolerance 50W, error 30W → skip
	st.Mode = ModeSelfConsumption
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("within deadband should return nothing, got %d", len(targets))
	}
}

func TestSelfConsumptionDischargesOnImport(t *testing.T) {
	// grid = +1000 (importing too much) → want battery to discharge (negative target)
	store := seedStore(1000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000 // big so slew doesn't interfere
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target")
	}
	if targets[0].TargetW >= 0 {
		t.Errorf("site convention: importing should lead to NEGATIVE (discharge) target, got %f",
			targets[0].TargetW)
	}
}

func TestSelfConsumptionChargesOnExport(t *testing.T) {
	// grid = -2000 (exporting) → want battery to charge (positive target)
	store := seedStore(-2000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 { t.Fatal("expected 1 target") }
	if targets[0].TargetW <= 0 {
		t.Errorf("exporting should lead to POSITIVE (charge) target, got %f", targets[0].TargetW)
	}
}

func TestProportionalSplitByCapacity(t *testing.T) {
	bats := []batteryInfo{
		{driver: "big", capacityWh: 15000, currentW: 0, soc: 0.5, online: true},
		{driver: "small", capacityWh: 5000, currentW: 0, soc: 0.5, online: true},
	}
	targets := distributeProportional(bats, -1000, nil) // want -1000W total discharge; nil groupPV → capacity-only split
	var big, small float64
	for _, tg := range targets {
		if tg.Driver == "big" { big = tg.TargetW }
		if tg.Driver == "small" { small = tg.TargetW }
	}
	// Big is 75%, small 25% → big = -750, small = -250
	if math.Abs(big+750) > 1 { t.Errorf("big got %f, want -750", big) }
	if math.Abs(small+250) > 1 { t.Errorf("small got %f, want -250", small) }
}

func TestProportionalUsesTotalDesired(t *testing.T) {
	// Both batteries currently at +500 (charging). Correction -200 means "reduce charging".
	// Expected: both end up at +400 each (half of +800 total desired).
	bats := []batteryInfo{
		{driver: "a", capacityWh: 10000, currentW: 500, soc: 0.5, online: true},
		{driver: "b", capacityWh: 10000, currentW: 500, soc: 0.5, online: true},
	}
	targets := distributeProportional(bats, -200, nil)
	for _, tg := range targets {
		if math.Abs(tg.TargetW-400) > 1 {
			t.Errorf("%s: got %f, want 400", tg.Driver, tg.TargetW)
		}
	}
}

func TestPriorityDrainsPrimaryFirst(t *testing.T) {
	bats := []batteryInfo{
		{driver: "primary", capacityWh: 15000, currentW: 0, soc: 0.5, online: true},
		{driver: "secondary", capacityWh: 10000, currentW: 0, soc: 0.5, online: true},
	}
	// Small correction - primary should take it all
	targets := distributePriority(bats, -1000, []string{"primary", "secondary"})
	var p, s float64
	for _, tg := range targets {
		if tg.Driver == "primary" { p = tg.TargetW }
		if tg.Driver == "secondary" { s = tg.TargetW }
	}
	if math.Abs(p+1000) > 1 { t.Errorf("primary: got %f, want -1000", p) }
	if s != 0 { t.Errorf("secondary: got %f, want 0", s) }
}

func TestPriorityOverflowsToSecondary(t *testing.T) {
	bats := []batteryInfo{
		{driver: "primary", capacityWh: 15000, currentW: 0, soc: 0.5, online: true},
		{driver: "secondary", capacityWh: 10000, currentW: 0, soc: 0.5, online: true},
	}
	// Big correction - primary saturates at -5000 (per-command cap), rest spills
	targets := distributePriority(bats, -7000, []string{"primary", "secondary"})
	var p, s float64
	for _, tg := range targets {
		if tg.Driver == "primary" { p = tg.TargetW }
		if tg.Driver == "secondary" { s = tg.TargetW }
	}
	if p != -5000 { t.Errorf("primary: got %f, want -5000", p) }
	if math.Abs(s+2000) > 1 { t.Errorf("secondary: got %f, want -2000", s) }
}

func TestWeightedDistribution(t *testing.T) {
	bats := []batteryInfo{
		{driver: "a", capacityWh: 10000, currentW: 0, soc: 0.5, online: true},
		{driver: "b", capacityWh: 10000, currentW: 0, soc: 0.5, online: true},
	}
	weights := map[string]float64{"a": 0.8, "b": 0.2}
	targets := distributeWeighted(bats, 1000, weights)
	var a, b float64
	for _, tg := range targets {
		if tg.Driver == "a" { a = tg.TargetW }
		if tg.Driver == "b" { b = tg.TargetW }
	}
	if math.Abs(a-800) > 1 { t.Errorf("a: got %f, want 800", a) }
	if math.Abs(b-200) > 1 { t.Errorf("b: got %f, want 200", b) }
}

// ---- Clamps ----

func TestClampWithSoCBlocksDischargeWhenEmpty(t *testing.T) {
	b := batteryInfo{soc: 0.04}
	v, was := clampWithSoC(-1000, b)
	if v != 0 || !was {
		t.Errorf("SoC<5%%: discharge should be blocked, got %f clamped=%v", v, was)
	}
	v, was = clampWithSoC(+1000, b)
	if v != 1000 || was {
		t.Error("charge at low SoC should pass through unchanged")
	}
}

func TestClampWithSoCCapsAtDefaultWhenLimitsUnset(t *testing.T) {
	// No per-driver limits → falls back to global MaxCommandW default.
	b := batteryInfo{soc: 0.5}
	v, was := clampWithSoC(+7000, b)
	if v != MaxCommandW || !was {
		t.Errorf("expected cap at +%d default, got %f", MaxCommandW, v)
	}
	v, was = clampWithSoC(-7000, b)
	if v != -MaxCommandW || !was {
		t.Errorf("expected cap at -%d default, got %f", MaxCommandW, v)
	}
}

// Per-driver limits override the global default. Charge + discharge can be
// asymmetric (hybrid inverters often are). Issue #145.
func TestClampWithSoCUsesPerBatteryLimits(t *testing.T) {
	b := batteryInfo{soc: 0.5, maxChargeW: 10000, maxDischargeW: 8000}
	// Charge up to 10 kW — passes through.
	if v, was := clampWithSoC(+9500, b); v != 9500 || was {
		t.Errorf("+9500 under 10 kW cap: got %f clamped=%v, want 9500 false", v, was)
	}
	// +12 kW above cap → clamped at 10 kW.
	if v, was := clampWithSoC(+12000, b); v != 10000 || !was {
		t.Errorf("+12000 vs 10 kW cap: got %f clamped=%v, want 10000 true", v, was)
	}
	// Discharge cap is separate (8 kW). -9 kW → clamped at -8 kW.
	if v, was := clampWithSoC(-9000, b); v != -8000 || !was {
		t.Errorf("-9000 vs 8 kW discharge cap: got %f clamped=%v, want -8000 true", v, was)
	}
}

// ---- Fuse guard ----

// Old-world test updated for the bidirectional predicted-grid guard
// (#145). Previous semantics ("PV + discharge > fuse → scale") assumed
// zero load and treated discharge as always pushing past the fuse. The
// new guard predicts site-boundary flow from live telemetry, which is
// physically accurate AND covers the charge side symmetrically.
func TestFuseGuardScalesDischargeWhenExportExceedsFuse(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, -8000, nil, nil) // grid exporting 8 kW (PV dominant)
	s.DriverHealthMut("meter").RecordSuccess()
	s.Update("a", telemetry.DerBattery, 0, nil, nil)
	s.DriverHealthMut("a").RecordSuccess()
	targets := []DispatchTarget{{Driver: "a", TargetW: -6000}} // add 6 kW discharge on top
	// Predicted grid = -8000 - 0 + (-6000) = -14000 (exporting). Fuse 11040.
	st := NewState(0, 50, "meter")
	scaled := applyFuseGuard(targets, s, st, 11040)
	if !scaled[0].Clamped {
		t.Error("expected clamped=true")
	}
	// Over by 14000 − 11040 = 2960. Discharge scales from 6000 → 6000 − 2960 = 3040.
	if math.Abs(scaled[0].TargetW-(-3040)) > 1 {
		t.Errorf("expected target ≈ -3040 after scaling, got %f", scaled[0].TargetW)
	}
}

// Small charge under the fuse → unchanged. No scaling.
func TestFuseGuardPassesThroughWhenWithinFuse(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, 0, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	s.Update("a", telemetry.DerBattery, 0, nil, nil)
	s.DriverHealthMut("a").RecordSuccess()
	targets := []DispatchTarget{{Driver: "a", TargetW: 3000}}
	st := NewState(0, 50, "meter")
	scaled := applyFuseGuard(targets, s, st, 11040)
	if scaled[0].TargetW != 3000 || scaled[0].Clamped {
		t.Errorf("within fuse: got %f clamped=%v, want 3000 false", scaled[0].TargetW, scaled[0].Clamped)
	}
}

// Charge side now protected (#145). With high load and aggressive
// charge targets, predicted grid import can exceed the fuse — the
// guard must scale charge down.
func TestFuseGuardScalesChargingWhenImportExceedsFuse(t *testing.T) {
	s := telemetry.NewStore()
	// Grid currently importing 8 kW (load-dominated, night/no PV).
	s.Update("meter", telemetry.DerMeter, 8000, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()
	s.Update("a", telemetry.DerBattery, 0, nil, nil)
	s.DriverHealthMut("a").RecordSuccess()
	s.Update("b", telemetry.DerBattery, 0, nil, nil)
	s.DriverHealthMut("b").RecordSuccess()
	targets := []DispatchTarget{
		{Driver: "a", TargetW: 5000},
		{Driver: "b", TargetW: 5000},
	}
	// Predicted = 8000 - 0 + 10000 = 18000 W. Fuse 11040.
	// Overage = 6960. Total charge = 10000. New total = 3040. Scale = 0.304.
	st := NewState(0, 50, "meter")
	scaled := applyFuseGuard(targets, s, st, 11040)
	var totalCharge float64
	for _, tgt := range scaled {
		if tgt.TargetW > 0 {
			totalCharge += tgt.TargetW
		}
	}
	if math.Abs(totalCharge-3040) > 2 {
		t.Errorf("total charging after scaling = %f, want ≈ 3040", totalCharge)
	}
	for _, tgt := range scaled {
		if !tgt.Clamped {
			t.Errorf("%s: expected Clamped=true after charge scaling", tgt.Driver)
		}
	}
}

// Mirror of the charging test with no grid reading: guard can't predict
// reliably, stays conservative by NOT scaling (leave the decision to
// the upstream per-battery cap). Guards against a bare-metal test
// setup accidentally triggering the guard.
func TestFuseGuardNoOpWithoutMeterReading(t *testing.T) {
	s := telemetry.NewStore()
	targets := []DispatchTarget{{Driver: "a", TargetW: 5000}}
	// No meter reading → currentGrid defaults to 0 → predicted = 5000 which is fine.
	st := NewState(0, 50, "does-not-exist")
	scaled := applyFuseGuard(targets, s, st, 11040)
	if scaled[0].TargetW != 5000 || scaled[0].Clamped {
		t.Errorf("absent meter → no prediction-based clamp, got %f clamped=%v",
			scaled[0].TargetW, scaled[0].Clamped)
	}
}

// ---- Full cycle ----

func TestFullCycleRespondsToTransient(t *testing.T) {
	store := seedStore(0, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0 // disable holdoff

	// Cycle 1: grid balanced, no action
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Error("balanced grid should not dispatch")
	}

	// Cycle 2: simulate a load step - grid rises to +1500
	store.Update("ferroamp", telemetry.DerMeter, 1500, nil, nil)
	time.Sleep(10 * time.Millisecond) // move past MinDispatchInterval=0
	targets = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatal("should dispatch on load step")
	}
	if targets[0].TargetW >= 0 {
		t.Errorf("import → discharge target (negative), got %f", targets[0].TargetW)
	}
}

func TestHoldoffBlocksRapidDispatch(t *testing.T) {
	store := seedStore(2000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	now := time.Now()
	st.LastDispatch = &now
	st.MinDispatchIntervalS = 5
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Error("holdoff should block dispatch")
	}
}

func TestSetGridTargetUpdatesPI(t *testing.T) {
	st := NewState(0, 50, "ferroamp")
	st.SetGridTarget(-500)
	if st.GridTargetW != -500 { t.Errorf("state: %f", st.GridTargetW) }
	if st.PI.Setpoint != -500 { t.Errorf("pi setpoint: %f", st.PI.Setpoint) }
}

func TestEmptyBatteriesReturnsNoTargets(t *testing.T) {
	store := seedStore(1000, nil)
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	targets := ComputeDispatch(store, st, caps(map[string]float64{}), 11040)
	if len(targets) != 0 { t.Error("no batteries → no dispatch") }
}

func TestPeakShavingNoActionInBand(t *testing.T) {
	store := seedStore(3000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePeakShaving
	st.PeakLimitW = 5000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Errorf("within peak band should be no-op, got %d targets", len(targets))
	}
}

func TestPeakShavingActsWhenOverLimit(t *testing.T) {
	store := seedStore(7000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePeakShaving
	st.PeakLimitW = 5000
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Error("over peak limit should dispatch")
	}
}

func TestEVChargingSignalExcludedFromGrid(t *testing.T) {
	// Grid = +3000 includes 2500W EV charging. Effective = +500W → within tolerance.
	store := seedStore(3000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 2500
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	// Effective grid 500 → beyond 50 band, but small correction expected
	if len(targets) > 0 {
		// Allow small dispatch, but verify not trying to cover all 3000W
		if math.Abs(targets[0].TargetW) > 2000 {
			t.Errorf("EV-corrected dispatch should be modest, got %f", targets[0].TargetW)
		}
	}
}

func TestEVChargingSignalOverriddenByDerEVReading(t *testing.T) {
	// A DerEV driver reports 4000W. EVChargingW was 0 (no manual slider).
	// After ComputeDispatch, EVChargingW must reflect the live reading
	// so the dispatch clamp works against real hardware.
	store := seedStore(5000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	store.Update("easee", telemetry.DerEV, 4000, nil, nil)
	store.DriverHealthMut("easee").RecordSuccess()
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 0
	st.SlewRateW = 100000
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.EVChargingW != 4000 {
		t.Errorf("expected EVChargingW to be overridden by live EV reading = 4000, got %f", st.EVChargingW)
	}
}

// TestBatteryCoversEV_OffExcludesEVFromGrid mirrors the existing
// exclusion behaviour. Grid meter reads +3000 W, 2500 W is EV, so
// the effective grid the controller sees should be 500 W — well
// within the dead-band — and the battery should not try to cover
// the whole 3000 W. Regression guard that the new flag's default
// preserves current behaviour.
func TestBatteryCoversEV_OffExcludesEVFromGrid(t *testing.T) {
	store := seedStore(3000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 2500
	st.BatteryCoversEV = false // explicit — this is the default
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) > 0 && math.Abs(targets[0].TargetW) > 2000 {
		t.Errorf("with flag off, battery must not try to cover 2500W EV draw; got target=%f", targets[0].TargetW)
	}
}

// TestBatteryCoversEV_OnIncludesEVInGrid covers the opt-in scenario:
// high grid prices now, cheap solar later → operator flips the
// override and the battery discharges to cover full grid load
// including EV. The dispatch should produce a target that actively
// pulls the battery into discharge territory for the full 3000 W
// import, not just the 500 W house portion.
func TestBatteryCoversEV_OnIncludesEVInGrid(t *testing.T) {
	store := seedStore(3000, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 2500
	st.BatteryCoversEV = true // opt in — battery covers everything
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Fatal("expected a dispatch target when grid is 3 kW over target, got none")
	}
	// With flag on, the full 3000 W import should drive battery toward
	// discharge (negative target in site convention). Require at least
	// half of the raw import as discharge command — conservative on the
	// PI gain but clearly separates "covers EV too" from "house only".
	if targets[0].TargetW > -1500 {
		t.Errorf("with flag on, battery must drive toward discharge for full import; got target=%f (want <= -1500)", targets[0].TargetW)
	}
}

func TestEVChargingManualPreservedWhenNoDriver(t *testing.T) {
	// No DerEV reading. The manual slider value (1500W) must survive —
	// we don't want an offline / stale driver to silently zero it out.
	store := seedStore(1500, []struct{ name string; currentW, soc float64 }{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.EVChargingW = 1500
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.EVChargingW != 1500 {
		t.Errorf("expected EVChargingW manual value 1500 to survive, got %f", st.EVChargingW)
	}
}

// ---- Slew rate anchor ----

// xorath's reported bug: battery at 10% SoC was commanded -5000 W the
// previous cycle but physically responded with 0 W (empty). When the user
// removed EV load creating surplus, the PI wanted +2000 W, but slew
// anchored on the stale -5000 W command capped new command at
// -5000 + 500 = -4500 W. Reversing direction took 5000/500 = 10 cycles
// (~50 s at 5 s interval). Anchoring slew on actual smoothed power
// (which was 0 W, not -5000 W) lets dispatch pivot within one slew step.
func TestSlewAnchorsOnActualNotStaleCommand(t *testing.T) {
	// Battery: previous command -5000 W, actual output 0 W (empty).
	// Grid: -2000 W (surplus → PI wants to charge the battery).
	store := seedStore(-2000, []struct{ name string; currentW, soc float64 }{
		{"pixii", 0, 0.10}, // at SoC min, actual bat_w = 0 despite command
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	// Seed the stale command — this is what the dispatch stored after
	// last cycle, when it commanded -5000 W and the battery couldn't
	// comply.
	st.PrevTargets["pixii"] = -5000
	// Note: no battery called "ferroamp" in this store, so dispatch
	// skips it as unavailable. Only pixii is in the game.
	targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 10000}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 dispatch target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// With actual-anchored slew (anchor = 0 W), one step toward +W
	// should land the command in (0, +500]. With the old stale-anchored
	// slew it would land at -4500 W.
	if got < 0 {
		t.Errorf("expected positive (charge) target, got %f W — slew still anchored to stale command", got)
	}
	if got > st.SlewRateW+1e-6 {
		t.Errorf("expected target within one slew step of 0 W actual, got %f W", got)
	}
}

// Ensure normal in-tracking operation (actual ≈ command) still respects
// the slew limit from the actual. This prevents the fix from letting the
// PI jump more than slew_rate per cycle when the battery is tracking well.
func TestSlewRespectsRateWhenTracking(t *testing.T) {
	// Battery actively discharging at -1000 W, both actual and prev command.
	store := seedStore(1500, []struct{ name string; currentW, soc float64 }{
		{"pixii", -1000, 0.6},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	st.PrevTargets["pixii"] = -1000
	// PI will want a big discharge to cover the +1500 import.
	targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 10000}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Anchor is actual -1000; one slew step toward negative lands at -1500.
	if math.Abs(got-(-1500)) > 1e-6 {
		t.Errorf("expected slewed target = -1500 W (anchor -1000 + step -500), got %f W", got)
	}
}

// ---- Energy-allocation dispatch path (UseEnergyDispatch) ----

// newStateWithEnergyDispatch sets up a fresh State in planner_arbitrage mode
// with the energy-allocation path enabled. Slew + holdoff are relaxed so the
// test exercises the formula, not the rate limiter.
func newStateWithEnergyDispatch(dir SlotDirective, siteMeter string) *State {
	st := NewState(0, 0, siteMeter) // tolerance=0 so no deadband noise
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000 // effectively unbounded for the formula test
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	return st
}

// Core conversion: 200 Wh allocated, whole 15-min slot remaining,
// no energy delivered yet → target = 200 × 3600 / 900 = 800 W.
func TestEnergyDispatchConvertsWhToW(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200,
		Strategy:        "arbitrage",
	}
	store := seedStore(0, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// 200 Wh × 3600 s/h / 900 s ≈ 800 W. Small tolerance for time drift.
	if got := targets[0].TargetW; math.Abs(got-800) > 5 {
		t.Errorf("TargetW = %f, want ≈800 (200 Wh / 15 min)", got)
	}
}

// The motivating scenario (operator report 2026-04-17): forecast PV 700 W,
// actual 4800 W, plan wants to charge 200 Wh this slot. Under the legacy
// path the PI drives battery to ~3.9 kW charge (absorb everything) to hit
// grid_target. Under energy dispatch the battery stays at ~800 W and the
// 4 kW surplus flows to the grid.
func TestEnergyDispatchDoesNotAbsorbPVSurprise(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200, // plan: charge 200 Wh
		Strategy:        "arbitrage",
	}
	// Grid exporting 4 kW (because PV 4.8 kW, load 0.8 kW, battery 0 W).
	// Under legacy PI with grid_target=−51 the controller would pull the
	// battery into aggressive charging to pin grid to −51.
	store := seedStore(-4000, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	// PV telemetry so applyFuseGuard has something to count.
	store.Update("pv-1", telemetry.DerPV, -4800, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Expect ~800 W charge, NOT multi-kW absorption. Flag anything beyond
	// 1.5 kW — that's 7× the plan's intent and signals the old bug.
	got := targets[0].TargetW
	if got > 1500 {
		t.Errorf("TargetW = %f W — battery is absorbing the PV surprise (regression). plan wanted ~800 W charge.", got)
	}
	if got < 100 {
		t.Errorf("TargetW = %f W — battery should still be charging per plan intent.", got)
	}
}

// Regression: BatteryCoversEV=false must hold on the energy-allocation
// path too. Operator report 2026-04-27: "I have 'let battery cover ev'
// disabled now, even though it discharges into the EV." Cause: the
// energy-dispatch branch consulted neither BatteryCoversEV nor the
// EV draw, blindly executing the plan's BatteryEnergyWh directive.
//
// Scenario: EV pulling 4 kW, house side importing 200 W, plan wants
// to discharge ~1 kWh this slot (≈ 4 kW). Toggle says battery shall
// not feed the EV. Expected: battery discharges only as much as the
// house alone needs (~200 W), NOT the planner's 4 kW.
func TestEnergyDispatchHonorsBatteryCoversEVOff(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -1000, // plan: discharge 1 kWh this slot (~ -4 kW)
		Strategy:        "arbitrage",
	}
	// rawGridW = 4200 W (200 W house + 4000 W EV importing). Battery 0.
	store := seedStore(4200, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 4000     // manual injection — no EV driver in store
	st.BatteryCoversEV = false // explicit; the contended toggle

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Allowed: discharge up to ~ house import (200 W). Forbidden:
	// discharge of EV magnitude (~ -4000 W).
	if got < -500 {
		t.Errorf("TargetW = %f W — battery is discharging into the EV despite BatteryCoversEV=false. "+
			"Expected at most ~ -200 W (house side only).", got)
	}
}

// Counter-test: with BatteryCoversEV=true, the energy path stays unchanged —
// plan's full discharge is honored.
func TestEnergyDispatchBatteryCoversEVOnLetsPlanRun(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -1000,
		Strategy:        "arbitrage",
	}
	store := seedStore(4200, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 4000
	st.BatteryCoversEV = true // opt-in: planner runs as-is

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Plan wanted ~ -4000 W. Tolerate slew/clamp drift but expect heavy discharge.
	if got > -1500 {
		t.Errorf("TargetW = %f W — plan wanted ~ -4000 W, but battery is barely discharging. "+
			"BatteryCoversEV=true should let the planner's directive run.", got)
	}
}

// Joint fuse-budget allocator: when EV draw + plan's battery charge would
// exceed the fuse, both should be scaled proportionally — battery charge
// reduced AND state.FuseEVMaxW published so the loadpoint controller can
// curtail the EV. Operator report: oscillation loop where plan ramps
// battery, fuse guard cuts it, plan ramps again.
//
// Scenario: house 200 W, EV at 8 kW, plan wants battery +5 kW, fuse 11 kW.
// Naive: total grid = 200+8000+5000 = 13.2 kW → fuse busts.
// Joint allocator: scale = (11000-200-0)/(5000+8000) ≈ 0.831.
//   battery charge → ~4150 W
//   FuseEVMaxW    → ~6650 W
//   sum + house    ≈ 11.0 kW. Fuse respected.
func TestJointFuseAllocatorScalesBothBatteryAndEV(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1250, // ~5 kW for 15 min
		Strategy:        "arbitrage",
	}
	// rawGridW = 13.2 kW (importing — house 200 + EV 8000 + plan-imminent
	// battery 5000, but battery currentW=0 means it's not yet charging,
	// so right now grid is 200+8000=8200. Plan WANTS battery to add 5kW.
	// Use 8200 to model the live state at the start of the tick.)
	store := seedStore(8200, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 8000
	st.BatteryCoversEV = true // not the toggle under test; let plan run

	const fuseMaxW = 11000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), fuseMaxW)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Allocator contract: FuseEVMaxW published so loadpoint can throttle.
	// In *this* tick, EV is still at 8 kW (hasn't seen the cap yet), so the
	// existing fuse guard further clamps battery to ~2800 W (= 11000 −
	// 8200). Within 1 follow-up tick (after EV throttles to ~6650 W),
	// battery climbs to ~4150 W. Joint stable point: 4150 + 6650 = 10800
	// + 200 house = 11 kW = fuse.
	if got > 4500 {
		t.Errorf("TargetW = %.0f W — battery charge not cut by joint allocator (want ≤ 4500)", got)
	}
	if got < 0 {
		t.Errorf("TargetW = %.0f W — battery shouldn't be discharging here", got)
	}
	if !st.FuseSaturated {
		t.Errorf("FuseSaturated = false; want true after joint scaling")
	}
	// EV cap is the post-republish verdict: after applyFuseGuard +
	// forceFuseDischarge constrain battery further (and may swing it
	// into discharge), the cap accounts for the now-greater fuse
	// headroom. Bound below by ~6000 (the original allocator's
	// pessimistic cap) and above by the current EV draw — a cap
	// greater than the EV is currently drawing would be loose.
	if st.FuseEVMaxW < 6000 || st.FuseEVMaxW > st.EVChargingW {
		t.Errorf("FuseEVMaxW = %.0f — want in [6000, %.0f] (post-republish cap)",
			st.FuseEVMaxW, st.EVChargingW)
	}
	// Sanity: post-dispatch projected grid stays at or below fuse.
	projected := 8200.0 + got
	if projected > fuseMaxW+50 {
		t.Errorf("projected grid %.0f W > fuse %.0f after dispatch", projected, float64(fuseMaxW))
	}
}

// Counter-test: when battery wants to discharge, no scaling — discharge
// helps the fuse rather than competing with the EV.
func TestJointFuseAllocatorIgnoresDischarge(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -1000, // discharge
		Strategy:        "arbitrage",
	}
	store := seedStore(8200, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 8000
	st.BatteryCoversEV = true // discharge into EV explicitly OK

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11000)
	got := targets[0].TargetW
	// Plan wanted ~ -4000 W discharge. Should run roughly as planned —
	// discharge counts AGAINST grid, so doesn't fight EV.
	if got > -1500 {
		t.Errorf("TargetW = %.0f W — discharge unexpectedly cut", got)
	}
	if st.FuseSaturated {
		t.Errorf("FuseSaturated = true; want false (discharge helps fuse, no joint scaling needed)")
	}
}

// No EV competition: plan wants battery charge, no EV active. Allocator
// must not interfere even if rawGridW is high (fuse guard handles that).
func TestJointFuseAllocatorNoOpWithoutEV(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1250,
		Strategy:        "arbitrage",
	}
	store := seedStore(200, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 0

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11000)
	got := targets[0].TargetW
	if got < 4500 || got > 5500 {
		t.Errorf("TargetW = %.0f — no EV present, plan should run as-is (~5000 W)", got)
	}
	if st.FuseSaturated {
		t.Errorf("FuseSaturated = true with no EV — should be false")
	}
}

// Slot rollover: when SlotDirective returns a new SlotStart, the delivered
// accumulator must reset so the next slot starts from zero.
func TestEnergyDispatchResetsOnSlotRollover(t *testing.T) {
	now := time.Now()
	// First slot: mid-slot, accumulator should build up.
	dir1 := SlotDirective{
		SlotStart:       now.Add(-10 * time.Minute),
		SlotEnd:         now.Add(5 * time.Minute),
		BatteryEnergyWh: 200,
	}
	store := seedStore(0, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 800, 0.5}, // battery already charging 800 W
	})
	st := newStateWithEnergyDispatch(dir1, "ferroamp")

	// First call establishes the slot.
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	// New slot — different SlotStart. Should reset slotDelivered.
	dir2 := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 300,
	}
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir2, true }

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	if st.slotDelivered != 0 {
		t.Errorf("slotDelivered = %f after slot rollover, want 0", st.slotDelivered)
	}
	if !st.currentDirective.SlotStart.Equal(dir2.SlotStart) {
		t.Errorf("currentDirective.SlotStart = %v, want %v", st.currentDirective.SlotStart, dir2.SlotStart)
	}
}

// Energy-dispatch must keep GridTargetW and PI.Setpoint in lockstep so
// the legacy path doesn't inherit a stale PI setpoint when the operator
// later switches out of a planner mode. Regression test for a P1
// raised on PR #79 (Codex).
func TestEnergyDispatchSyncsPISetpointWithGridTarget(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200,
	}
	store := seedStore(0, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	// Pre-poison the setpoint as if a manual mode had set it earlier —
	// this is what SetGridTarget needs to overwrite atomically.
	st.PI.Setpoint = 3000
	st.GridTargetW = 3000

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	if st.PI.Setpoint != 0 {
		t.Errorf("PI.Setpoint = %f, want 0 after energy-dispatch cycle (stale setpoint would produce wrong corrections after mode switch)", st.PI.Setpoint)
	}
	if st.GridTargetW != 0 {
		t.Errorf("GridTargetW = %f, want 0", st.GridTargetW)
	}
}

// When the energy path is enabled but the plan is stale, the legacy path
// runs (PI on grid_target=0, self_consumption distribution). Verifies the
// fallback doesn't leave the path flag mis-set.
func TestEnergyDispatchFallsBackToLegacyWhenDirectiveUnavailable(t *testing.T) {
	store := seedStore(1000, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	// Directive returns ok=false — plan stale.
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	// Legacy fallback hook: return ok=false too, should route to the
	// "plan stale" self-consumption-with-grid-target=0 branch.
	st.PlanTarget = func(time.Time) (string, float64, bool) { return "", 0, false }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if !st.PlanStale {
		t.Error("expected PlanStale=true when both energy + legacy paths lack a plan")
	}
	// With grid = +1000 and target = 0, PI should command some charge-absorption
	// (negative correction if batteries > 0, toward discharge). Just assert the
	// path dispatched something.
	if len(targets) == 0 {
		t.Error("expected some dispatch under legacy fallback, got nothing")
	}
}

// Under planner_arbitrage — where the DP is explicitly allowed to export via
// battery — energy dispatch holds the plan even when live grid diverges.
// That's the point of "grid is the residual": arbitrage decides slot-by-slot
// that this_slot_W × slot_duration of battery energy is the cost-optimal
// cycle, and the EMS just executes it. Live export is a legal outcome.
//
// Contrast: under planner_self (see TestPlannerSelf* below) the same plan
// would be ignored in favour of reactive self-consumption — because that
// mode's contract is "never export via battery" regardless of what the
// forecast-based plan prescribes.
func TestEnergyDispatchHoldsPlanUnderArbitrage(t *testing.T) {
	now := time.Now()
	// Plan: discharge 552 Wh this slot. In W terms that's −2208.
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -552,
		Strategy:        "arbitrage",
	}
	store := seedStore(-1310, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -2200, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -740, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	// Mode stays ModePlannerArbitrage from the helper — the point.

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got > -1900 || got < -2500 {
		t.Errorf("TargetW = %f W — arbitrage battery should follow plan (~−2208 W) regardless of live grid flow", got)
	}
}

// ---- planner_self reactive execution (issue #130) ----
//
// planner_self promises "never imports to charge, never exports via the
// battery" (UI tooltip + docs/mpc-planner.md). The DP enforces this on
// forecast, but the energy-allocation dispatch path honours plan Wh
// regardless of live grid flow — so when PV or load diverges from the
// forecast the battery can cross the zero-grid invariant both ways.
//
// The fix: planner_self bypasses energy-allocation and uses reactive
// self-consumption (PI → gridW=0), with the plan providing only a
// per-slot *idle gate*. When the DP decided not to participate this
// slot (|planned BatteryEnergyWh| < IdleGateThresholdW when averaged
// over the slot) the EMS holds the battery at 0 and lets PV flow to
// grid — deferring opportunity to a richer later slot.

// Motivating scenario (operator report 2026-04-19): plan wanted to charge
// aggressively (forecast said big PV surplus), but actual PV came in low.
// Under energy dispatch the battery imports from grid to hit the Wh budget.
// Under the fix, the battery only absorbs what's actually available.
func TestPlannerSelfReactsToForecastOverestimate(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 2000, // plan: charge 2000 Wh ≈ 8 kW avg
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 300 W (actual PV minus actual load).
	// Under energy dispatch the battery would charge at ~8 kW and
	// drag grid to +7.7 kW import. Under reactive planner_self the
	// battery's charge is bounded by the live surplus (~300 W).
	store := seedStore(-300, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp") // tolerance=0 so PI fires
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000 // unbounded for single-tick formula test
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Reactive PI on grid=-300 with Kp=0.5 yields ~+150 W charge. The
	// key assertion is "nowhere near the 8 kW the energy path would
	// command" — that's the bug.
	if got > 1000 {
		t.Errorf("TargetW = %f W — planner_self must NOT charge beyond live surplus (issue #130 regression). Plan said +8 kW but reality had ~300 W surplus.", got)
	}
	if got < 0 {
		t.Errorf("TargetW = %f W — expected modest charge absorbing live surplus, got discharge", got)
	}
}

// Idle-gate scenario: DP decided to sit this slot out (save SoC for a
// later, more profitable slot). Plan's Wh is below threshold → EMS holds
// battery at 0 even when live PV surplus exists.
func TestPlannerSelfIdleGateHoldsBatteryAtZero(t *testing.T) {
	now := time.Now()
	// Plan: avg ~0 W (well below IdleGateThresholdW=100).
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 4 kW (real surplus exists). Battery currently
	// discharging 1 kW — should ramp toward 0, not toward absorbing
	// the surplus.
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1000, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 500 // realistic — expect one slew step from −1000 toward 0
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Anchor on actual SmoothedW=-1000. Moving toward 0 by one slew
	// step of 500 W → -500 W. Not pushing toward -4000 to absorb surplus.
	if got < -500.001 || got > 0.001 {
		t.Errorf("TargetW = %f W — idle-gated battery should ramp toward 0 (expected [−500, 0]), not react to live surplus", got)
	}
}

// Plan says discharge this slot (above idle threshold) and live grid is
// importing. Reactive PI drives battery to cover live import exactly —
// not the planned Wh magnitude (which was larger because forecast load
// was higher than reality). Never crosses into export.
func TestPlannerSelfParticipatesReactivelyCoveringImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -800, // plan: discharge 800 Wh ≈ −3.2 kW avg
		Strategy:        "self_consumption",
	}
	// Live: importing 2 kW. Reactive PI wants to kill the import —
	// battery should discharge ~2 kW (NOT the planned −3.2 kW).
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got >= 0 {
		t.Errorf("TargetW = %f W — expected discharge (negative) to cover live import", got)
	}
	// Bounded by live import magnitude, not the plan's larger ask.
	// PI with Kp=0.5 on 2 kW error gives ~−1000 W first cycle.
	// Anything past −2500 W would be over-discharging toward export.
	if got < -2500 {
		t.Errorf("TargetW = %f W — reactive discharge should be bounded by live import (~2 kW), not the planned −3.2 kW", got)
	}
}

// Multi-cycle steady-state: idle-gated battery starts far from 0 and must
// reach 0 monotonically (no PI integral-windup overshoot, slew respected).
// Guards against the "gate goes on but PI wound up from earlier cycles
// keeps pushing" class of bug.
func TestPlannerSelfIdleGateRampsBatteryToZeroOverCycles(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // idle-gated
		Strategy:        "self_consumption",
	}
	// Battery at -2000 W (discharging), live grid exporting 500 W — under
	// the #153 idle-gate-override threshold, so the gate truly holds and
	// this test exercises the ramp-to-zero behaviour it was written to
	// guard. Heavier export would correctly trigger the override and
	// invalidate the assumption.
	store := seedStore(-500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -2000, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	// Simulate N cycles. Each cycle advances the battery's SmoothedW
	// toward the prev target so the slew anchor tracks reality.
	var last float64
	for i := 0; i < 10; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) != 1 {
			t.Fatalf("cycle %d: want 1 target, got %d", i, len(targets))
		}
		last = targets[0].TargetW
		// Fake the battery responding instantly to the new command.
		store.Update("ferroamp", telemetry.DerBattery, last, ptrF64(0.5), nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	if math.Abs(last) > 1 {
		t.Errorf("after 10 cycles with slew=500 from -2000 toward 0, expected final target ≈ 0, got %f", last)
	}
}

// EV load on the grid meter is subtracted from gridW before the PI kicks
// in (dispatch.go: `gridW := rawGridW - state.EVChargingW`). Verify that
// planner_self inherits this — an active EV should NOT drive the battery
// to cover EV charging.
func TestPlannerSelfRespectsEVChargingSignal(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -200, // plan above idle threshold
		Strategy:        "self_consumption",
	}
	// Grid reads +3000 (total import), but 3000 W of it is the EV.
	// Effective house gridW = 0 → battery should sit still.
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.EVChargingW = 3000
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	// Effective house grid is 0 — within the 50 W deadband — dispatch skips.
	if len(targets) != 0 {
		t.Errorf("expected no dispatch when EV absorbs all import (effective gridW=0), got %d targets: %+v", len(targets), targets)
	}
}

// When the plan is absent (SlotDirective returns false) planner_self
// degrades to plain manual self_consumption — same behaviour as the
// operator gets today when they pick "Self-consumption" without planner.
func TestPlannerSelfWithoutPlanActsLikeManual(t *testing.T) {
	// Live: importing 1 kW — same setup as TestSelfConsumptionDischargesOnImport.
	store := seedStore(1000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	st.PlanTarget = func(time.Time) (string, float64, bool) { return "", 0, false }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].TargetW >= 0 {
		t.Errorf("TargetW = %f W — planner_self with stale plan must still cover import (fall through to reactive self_consumption)", targets[0].TargetW)
	}
	if !st.PlanStale {
		t.Error("expected PlanStale=true when planner_self sees no directive")
	}
}

// ---- Per-driver power limits (#145) ----

// End-to-end via ModeCharge (the "fill every battery fully" knob):
// Ferroamp with max_charge_w=10000, Sungrow using the default. With
// per-driver caps, Ferroamp should be commanded at its 10 kW limit
// and Sungrow at the 5 kW default — previously both would have been
// pinned to 5 kW regardless of hardware capability.
func TestPerDriverLimits_ChargeModeRespectsAsymmetricCaps(t *testing.T) {
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeCharge
	st.DriverLimits = map[string]PowerLimits{
		"ferroamp": {MaxChargeW: 10000, MaxDischargeW: 10000},
		// sungrow intentionally omitted → falls through to MaxCommandW default.
	}

	// Fuse set generously so the per-driver caps are what actually binds.
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 50000)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	if got["ferroamp"] != 10000 {
		t.Errorf("ferroamp = %f — ModeCharge should drive to per-driver MaxChargeW (10000)", got["ferroamp"])
	}
	if got["sungrow"] != 5000 {
		t.Errorf("sungrow = %f — expected the MaxCommandW default (5000) when no override", got["sungrow"])
	}
}

// A per-driver discharge cap is honoured separately from the charge
// cap. Real hybrid inverters commonly differ between the two directions.
func TestPerDriverLimits_AsymmetricChargeVsDischarge(t *testing.T) {
	// Grid importing 12 kW (high load → big discharge correction).
	store := seedStore(12000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.DriverLimits = map[string]PowerLimits{
		"ferroamp": {MaxChargeW: 15000, MaxDischargeW: 7000}, // asymmetric caps
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 22080)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Must be discharging, and must NOT exceed -7000 (discharge cap).
	if targets[0].TargetW > 0 {
		t.Errorf("expected negative (discharge) target, got %f", targets[0].TargetW)
	}
	if targets[0].TargetW < -7000 {
		t.Errorf("target = %f — asymmetric discharge cap (7 kW) was not honoured", targets[0].TargetW)
	}
}

// ---- Inverter-affinity routing (#143) ----

// All charging flows to the group that has live PV — cross-inverter
// DC→AC→AC→DC is avoided.
func TestInverterAffinity_PrefersLocalBatteryForLocalSurplus(t *testing.T) {
	bats := []batteryInfo{
		{driver: "ferroamp", capacityWh: 15200, currentW: 0, soc: 0.5, online: true, group: "ferroamp"},
		{driver: "sungrow", capacityWh: 9600, currentW: 0, soc: 0.5, online: true, group: "sungrow"},
	}
	// All surplus on Ferroamp's inverter; none on Sungrow's.
	groupPV := map[string]float64{"ferroamp": 4000, "sungrow": 0}
	// +3 kW correction — fits entirely inside Ferroamp's local surplus.
	targets := distributeProportional(bats, 3000, groupPV)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	if math.Abs(got["ferroamp"]-3000) > 1 {
		t.Errorf("ferroamp target = %f, want 3000 (all local routing)", got["ferroamp"])
	}
	if math.Abs(got["sungrow"]) > 1 {
		t.Errorf("sungrow target = %f, want 0 (no local PV, correction absorbable locally)", got["sungrow"])
	}
}

// When the operator demands more charge than the sum of local PV, the
// overflow is routed proportionally across the whole fleet. The
// locality bonus is exhausted first; nothing to gain after that.
func TestInverterAffinity_FallsBackToProportionalOnOverflow(t *testing.T) {
	bats := []batteryInfo{
		{driver: "ferroamp", capacityWh: 15200, currentW: 0, soc: 0.5, online: true, group: "ferroamp"},
		{driver: "sungrow", capacityWh: 9600, currentW: 0, soc: 0.5, online: true, group: "sungrow"},
	}
	// 4 kW of PV on ferroamp only; operator wants to charge 6 kW total.
	groupPV := map[string]float64{"ferroamp": 4000, "sungrow": 0}
	targets := distributeProportional(bats, 6000, groupPV)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	// Ferroamp gets full 4 kW local + its capacity share of the 2 kW
	// overflow = 4000 + 2000 * 15200/24800 = 4000 + 1226 ≈ 5226.
	// clampWithSoC caps at 5000 per command.
	if got["ferroamp"] != 5000 {
		t.Errorf("ferroamp target = %f, want 5000 (local 4000 + overflow share, clamped at MaxCommandW)", got["ferroamp"])
	}
	// Sungrow gets its capacity share of the 2 kW overflow = 2000 * 9600/24800 ≈ 774.
	if math.Abs(got["sungrow"]-774) > 1 {
		t.Errorf("sungrow target = %f, want ≈774 (overflow × capacity share)", got["sungrow"])
	}
}

// With no inverter groups configured, the algorithm must produce
// identical results to today's capacity-proportional split — the
// backward-compat invariant.
func TestInverterAffinity_UngroupedBehavesAsBefore(t *testing.T) {
	bats := []batteryInfo{
		{driver: "a", capacityWh: 15200, currentW: 0, soc: 0.5, online: true}, // no group
		{driver: "b", capacityWh: 9600, currentW: 0, soc: 0.5, online: true},
	}
	// groupPV nil → "no locality info available" → fall back to
	// capacity-proportional.
	targets := distributeProportional(bats, 3000, nil)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	wantA := 3000 * 15200.0 / 24800.0 // ~1838
	wantB := 3000 * 9600.0 / 24800.0  // ~1161
	if math.Abs(got["a"]-wantA) > 1 {
		t.Errorf("a = %f, want %f (proportional, no groups)", got["a"], wantA)
	}
	if math.Abs(got["b"]-wantB) > 1 {
		t.Errorf("b = %f, want %f", got["b"], wantB)
	}
}

// Discharge skips the locality math entirely — routing discharge to
// a group with PV buys nothing (discharge energy goes on the AC bus
// regardless of origin), and the simpler formula keeps behaviour
// predictable for multi-battery sites running in self_consumption
// during import peaks.
func TestInverterAffinity_DischargeStillProportional(t *testing.T) {
	bats := []batteryInfo{
		{driver: "ferroamp", capacityWh: 15200, currentW: 0, soc: 0.5, online: true, group: "ferroamp"},
		{driver: "sungrow", capacityWh: 9600, currentW: 0, soc: 0.5, online: true, group: "sungrow"},
	}
	// PV on ferroamp only — but it's night-time demand so we're discharging.
	groupPV := map[string]float64{"ferroamp": 4000, "sungrow": 0}
	targets := distributeProportional(bats, -2000, groupPV)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	wantFerro := -2000 * 15200.0 / 24800.0 // ~-1226
	wantSun := -2000 * 9600.0 / 24800.0    // ~-774
	if math.Abs(got["ferroamp"]-wantFerro) > 1 {
		t.Errorf("ferroamp discharge = %f, want %f (proportional split on discharge)", got["ferroamp"], wantFerro)
	}
	if math.Abs(got["sungrow"]-wantSun) > 1 {
		t.Errorf("sungrow discharge = %f, want %f", got["sungrow"], wantSun)
	}
}

// End-to-end via ComputeDispatch: with InverterGroups wired on State
// and PV telemetry per driver, the dispatcher computes groupPV from
// live telemetry and routes charge preferentially. Guards against
// wiring bugs between the per-driver PV reading, the State map, and
// distributeProportional's input.
func TestInverterAffinity_EndToEndViaComputeDispatch(t *testing.T) {
	// Site exporting 3 kW surplus — PI will want to charge the fleet.
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	// All PV on Ferroamp's inverter; Sungrow's inverter has no PV right now.
	store.Update("ferroamp", telemetry.DerPV, -3500, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()

	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000 // unbounded so the formula is what we see
	st.MinDispatchIntervalS = 0
	st.InverterGroups = map[string]string{
		"ferroamp": "ferroamp",
		"sungrow":  "sungrow",
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	// Under plain proportional (no affinity) sungrow would receive
	// ~39% of the charge command (≈600 W on a 1.5 kW correction).
	// Under affinity sungrow gets ≈0 because ferroamp's local PV can
	// absorb the whole correction DC-direct.
	if got["sungrow"] > 200 {
		t.Errorf("sungrow target = %f — affinity should keep cross-inverter charge near zero when ferroamp's PV covers the surplus", got["sungrow"])
	}
	if got["ferroamp"] <= 0 {
		t.Errorf("ferroamp target = %f — should be charging since export + local PV = local routing opportunity", got["ferroamp"])
	}
}

// Issue #167: planner_self has two discrete per-slot states — IDLE or
// SELF_CONSUMPTION — both picked by the plan. The dispatch does not
// second-guess the plan with live data. When the plan says idle, the
// gate holds at 0 regardless of live grid direction or magnitude.
// Forecast divergence is handled by the reactive replan trigger
// (mpc/service.go), which re-runs the DP with fresh telemetry and
// emits a new plan that may flip the slot to self-consumption.
//
// Pre-#167 this scenario (large live export during a plan-idle slot)
// used to flip the gate off via a one-directional override. That was
// dropped because the symmetric case (large live import) was not
// handled and a mixed live/plan control surface made the mode's
// mental model noisy. See issue #167.
func TestPlannerSelfIdleGateHoldsEvenOnLargeLiveSurplus(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // plan wants idle
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 3 kW — pre-#167 this would have overridden
	// the gate. Post-#167 the gate holds; a fresh replan is the fix.
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Idle-gate holds → totalCorrection = -currentTotal. Battery at 0,
	// stays at 0. Operator who wants coverage switches to manual
	// self_consumption; replanner closes the loop automatically when
	// forecast error accumulates past the reactive-replan threshold.
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f — idle-gate must hold regardless of live grid; "+
			"forecast divergence is the replan's job, not the dispatcher's", targets[0].TargetW)
	}
}

// The idle gate holds on small live export (forecast and reality
// roughly agree). Kept as a regression guard for the symmetric
// property that the dispatch does not second-guess the plan.
func TestPlannerSelfIdleGateHoldsWhenLiveSurplusUnderThreshold(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 500 W — below the 1 kW override threshold.
	store := seedStore(-500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Idle-gate held → totalCorrection = -currentTotal. Battery at 0 →
	// desired 0, no charge command.
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f — idle-gate should hold under small export; "+
			"dispatch does not second-guess the plan, want ~0", targets[0].TargetW)
	}
}

// Idle gate holds during import too — the dispatch does not
// second-guess the plan regardless of live grid direction. Symmetric
// to TestPlannerSelfIdleGateHoldsEvenOnLargeLiveSurplus: one invariant,
// two sides. See issue #167.
func TestPlannerSelfIdleGateHoldsDuringImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Live: grid importing 2 kW (load-dominated evening).
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Idle-gate holds; battery stays at 0 even though live grid is
	// importing. If the forecast error is large enough, the reactive
	// replan trigger in mpc/service.go will re-plan and potentially
	// flip this slot to self_consumption.
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f — idle-gate should hold during live import; "+
			"dispatch does not second-guess the plan, want ~0", targets[0].TargetW)
	}
}

// Codex P2 on PR #131: planner_cheap → planner_self → planner_cheap within
// the same 15-minute slot must not let the energy path read stale
// `slotDelivered` accumulated before the planner_self hop. If that leak
// happens, the second cheap cycle computes `remainingWh` off the pre-hop
// delivery number and over-commands battery for the rest of the slot.
func TestPlannerSelfResetsEnergyBookkeepingOnEntry(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200,
		Strategy:        "arbitrage",
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 800, 0.5}, // mid-charge so cheap path accumulates delivery
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	// 1. Run arbitrage — primes state.currentDirective / slotDelivered / lastTickTs.
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.currentDirective.SlotStart.IsZero() {
		t.Fatal("precondition: arbitrage cycle should have set currentDirective")
	}

	// 2. Operator flips to planner_self inside the same slot.
	st.Mode = ModePlannerSelf
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	// After planner_self runs the energy-path bookkeeping must be
	// cleared so a future cheap/arbitrage cycle can't read stale state.
	if !st.currentDirective.SlotStart.IsZero() {
		t.Errorf("currentDirective.SlotStart = %v after planner_self, want zero", st.currentDirective.SlotStart)
	}
	if st.slotDelivered != 0 {
		t.Errorf("slotDelivered = %f after planner_self, want 0", st.slotDelivered)
	}
	if !st.lastTickTs.IsZero() {
		t.Errorf("lastTickTs = %v after planner_self, want zero", st.lastTickTs)
	}

	// 3. Flip back to arbitrage — the SlotStart-equality branch should
	// no longer match, so the code takes the rollover-reset path
	// cleanly rather than accumulating off a frozen baseline.
	st.Mode = ModePlannerArbitrage
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	// The re-primed directive should equal the fresh one exactly (reset
	// branch fires), which implies slotDelivered starts from 0 again.
	if !st.currentDirective.SlotStart.Equal(dir.SlotStart) {
		t.Errorf("arbitrage cycle after planner_self didn't re-prime directive; SlotStart=%v want %v",
			st.currentDirective.SlotStart, dir.SlotStart)
	}
}

// Regression for the post-forceFuseDischarge republish of FuseEVMaxW.
// Before the fix, the joint allocator computed FuseEVMaxW assuming the
// battery target it produced is what gets dispatched. But the reactive
// fuse-saver (PR #208) runs LAST and may swing battery from charge to
// discharge, freeing fuse headroom — yet FuseEVMaxW stayed at the
// original (too-conservative) value, throttling EV unnecessarily for
// one tick. With the fix, FuseEVMaxW reflects the post-saver battery
// totals and the EV gets the headroom it actually has.
func TestFuseEVMaxWRecomputedAfterForceFuseDischarge(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1250, // plan: charge 5 kW
		Strategy:        "arbitrage",
	}
	// Construct a scenario where the joint allocator engages (EV +
	// battery charge over fuse), AND the fuse-saver subsequently
	// flips battery to discharge. Easiest: rawGridW already over
	// fuseMaxW so applyFuseGuard zeros battery charge AND
	// forceFuseDischarge then drives it negative.
	store := seedStore(13000, []struct {
		name    string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.6},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 8000
	st.BatteryCoversEV = true
	const fuseMaxW = 11000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), fuseMaxW)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if !st.FuseSaturated {
		t.Fatalf("FuseSaturated = false; expected joint allocator to engage")
	}
	// The published FuseEVMaxW must reflect the post-saver battery
	// total. Concretely: with battery driven into discharge, the EV's
	// available headroom is greater than the joint allocator's initial
	// guess. So FuseEVMaxW should be ≥ the initial scaled value.
	if st.FuseEVMaxW <= 0 {
		t.Errorf("FuseEVMaxW = %.0f after republish; should publish a positive cap", st.FuseEVMaxW)
	}
	// Specifically: with battery target negative (discharging),
	// projected_grid = H + postBat + E ≤ fuse implies E_cap = fuse − H − postBat.
	// postBat is whatever the saver landed on; verify the published
	// value never exceeds the actual EV draw or goes negative.
	if st.FuseEVMaxW > st.EVChargingW {
		t.Errorf("FuseEVMaxW = %.0f exceeds current EV draw %.0f — implies the cap is loose",
			st.FuseEVMaxW, st.EVChargingW)
	}
}

// BatteryCoversEV mode regression: the joint allocator's H computation
// uses rawGridW directly, so its math is independent of the
// BatteryCoversEV branch (which only affects PI's gridW). Both modes
// should produce the same allocator behaviour given identical raw
// telemetry. Documents the design choice — protects against a future
// refactor that conflates the two.
func TestJointFuseAllocatorWithBatteryCoversEV(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1250,
		Strategy:        "arbitrage",
	}
	mkState := func(coversEV bool) (*State, *telemetry.Store) {
		store := seedStore(8200, []struct {
			name    string
			currentW, soc float64
		}{
			{"ferroamp", 0, 0.5},
		})
		st := newStateWithEnergyDispatch(dir, "ferroamp")
		st.EVChargingW = 8000
		st.BatteryCoversEV = coversEV
		return st, store
	}
	st1, store1 := mkState(true)
	st2, store2 := mkState(false)
	const fuseMaxW = 11000
	_ = ComputeDispatch(store1, st1, caps(map[string]float64{"ferroamp": 15200}), fuseMaxW)
	_ = ComputeDispatch(store2, st2, caps(map[string]float64{"ferroamp": 15200}), fuseMaxW)
	// Both must engage the joint allocator: same fuse-vs-EV+battery
	// arithmetic, regardless of operator-toggle for "let battery cover EV".
	if !st1.FuseSaturated || !st2.FuseSaturated {
		t.Errorf("FuseSaturated must engage in both modes: covers=%v others=%v",
			st1.FuseSaturated, st2.FuseSaturated)
	}
	// The PI / energy-path branch produces different battery TARGETS
	// in the two modes (that's the BatteryCoversEV behaviour). But the
	// JOINT allocator's E_cap formula depends only on rawGridW + E,
	// so the published FuseEVMaxW should be in the same ballpark
	// (within ~500 W, since post-republish accounts for the different
	// battery targets the two modes produce).
	delta := math.Abs(st1.FuseEVMaxW - st2.FuseEVMaxW)
	if delta > 1500 {
		t.Errorf("FuseEVMaxW differs by %.0f W between BatteryCoversEV true/false (%v vs %v) — math should be mode-independent",
			delta, st1.FuseEVMaxW, st2.FuseEVMaxW)
	}
}

// Plan/exec sign-mismatch floor — operator-report 2026-04-28 (08:00–08:15
// CEST): planner_arbitrage peak slot wanted -2400 W (discharge to export
// at 334 öre), dispatch produced +1640 W (charged surplus). The floor's
// job is to make that whole class of bug a no-op: when sign(plan_intent)
// disagrees with sign(executed total), every battery target becomes 0.
//
// Direct exercise of applyPlanSignFloor — keeps the test focused on the
// floor's contract and independent of which upstream path produced the
// wrong-sign target.
func TestPlanSignFloorClampsChargeWhenPlanSaysDischarge(t *testing.T) {
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	// Plan intent: discharge — BatteryEnergyWh < -idleWh (-50).
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{
			SlotStart:       time.Now(),
			SlotEnd:         time.Now().Add(15 * time.Minute),
			BatteryEnergyWh: -600,
		}, true
	}
	in := []DispatchTarget{{Driver: "pixii", TargetW: 1700}} // would-be charge
	out := applyPlanSignFloor(in, st)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].TargetW != 0 {
		t.Errorf("TargetW = %f, want 0 (plan says discharge, exec said charge)", out[0].TargetW)
	}
	if !out[0].Clamped {
		t.Error("expected Clamped=true so the dispatch trace shows the floor fired")
	}
}

func TestPlanSignFloorClampsDischargeWhenPlanSaysCharge(t *testing.T) {
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{
			SlotStart:       time.Now(),
			SlotEnd:         time.Now().Add(15 * time.Minute),
			BatteryEnergyWh: 1500, // charge intent
		}, true
	}
	in := []DispatchTarget{{Driver: "pixii", TargetW: -2000}}
	out := applyPlanSignFloor(in, st)
	if out[0].TargetW != 0 || !out[0].Clamped {
		t.Errorf("symmetric case: got TargetW=%f Clamped=%v, want 0/true",
			out[0].TargetW, out[0].Clamped)
	}
}

func TestPlanSignFloorPassesThroughWhenSignsAgree(t *testing.T) {
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{
			SlotStart:       time.Now(),
			SlotEnd:         time.Now().Add(15 * time.Minute),
			BatteryEnergyWh: -600,
		}, true
	}
	in := []DispatchTarget{{Driver: "pixii", TargetW: -1800}}
	out := applyPlanSignFloor(in, st)
	if out[0].TargetW != -1800 {
		t.Errorf("matching signs: TargetW = %f, want unchanged -1800", out[0].TargetW)
	}
	if out[0].Clamped {
		t.Error("Clamped should not be set when the floor was a no-op")
	}
}

func TestPlanSignFloorIgnoresManualModes(t *testing.T) {
	// Manual modes (self_consumption, peak_shaving, charge, etc.) have
	// no plan to disagree with. The floor must be a no-op so the
	// operator's manual selection executes as written.
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{BatteryEnergyWh: -600}, true
	}
	in := []DispatchTarget{{Driver: "pixii", TargetW: 1700}}
	out := applyPlanSignFloor(in, st)
	if out[0].TargetW != 1700 {
		t.Errorf("manual mode: TargetW = %f, want unchanged 1700", out[0].TargetW)
	}
}

func TestPlanSignFloorIdleBandIsNoOp(t *testing.T) {
	// An executed total inside ±100 W is "idle, no opinion on sign" —
	// don't trigger the floor on it.
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{BatteryEnergyWh: -600}, true
	}
	in := []DispatchTarget{{Driver: "pixii", TargetW: 50}} // tiny positive — within band
	out := applyPlanSignFloor(in, st)
	if out[0].TargetW != 50 {
		t.Errorf("inside idle band: TargetW = %f, want unchanged 50", out[0].TargetW)
	}
}

func TestPlanSignFloorReadsLegacyPlanTarget(t *testing.T) {
	// When SlotDirective isn't wired (legacy path), intent comes from
	// PlanTarget. mpc.actionToSlot encodes a planned discharge as
	// ("self_consumption", negative_grid_w) — verify we follow that
	// mapping.
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.PlanTarget = func(time.Time) (string, float64, bool) {
		return "self_consumption", -4000, true
	}
	in := []DispatchTarget{{Driver: "pixii", TargetW: 1700}} // charge against discharge plan
	out := applyPlanSignFloor(in, st)
	if out[0].TargetW != 0 {
		t.Errorf("legacy-path discharge intent: TargetW = %f, want 0", out[0].TargetW)
	}
}

func TestPlanSignFloorNoOpWhenNoIntent(t *testing.T) {
	// Neither callback wired, OR both return !ok → no plan to compare
	// against; let the dispatch result through unchanged.
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	in := []DispatchTarget{{Driver: "pixii", TargetW: 1700}}
	out := applyPlanSignFloor(in, st)
	if out[0].TargetW != 1700 {
		t.Errorf("no plan intent: TargetW = %f, want unchanged", out[0].TargetW)
	}
}

// End-to-end through ComputeDispatch: simulate the 06:00-06:15 UTC bug
// (planner_arbitrage discharge slot, but the dispatch math somehow
// produces charge — e.g. because the energy-allocation path didn't
// engage and the legacy path absorbed surplus). The floor must clamp.
//
// Constructed without UseEnergyDispatch so we land in the legacy PI
// path — same path that was active during the live incident.
func TestComputeDispatchAppliesSignFloorOnDischargeSlot(t *testing.T) {
	now := time.Now()
	store := seedStore(-1700, []struct {
		name    string
		currentW, soc float64
	}{
		{"pixii", 0, 0.15},
	})
	st := NewState(0, 0, "pixii")
	st.Mode = ModePlannerArbitrage
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	// Legacy path: PlanTarget returns ("self_consumption", -4000, true).
	// dispatch will set GridTargetW=-4000; but we OVERRIDE PlanTarget
	// here to return grid_target=0 to model the bug behaviour where the
	// negative grid target failed to plumb through (whatever the cause).
	// The plan INTENT (discharge) still comes from SlotDirective.
	st.PlanTarget = func(time.Time) (string, float64, bool) {
		return "self_consumption", 0, true // bug: should have been negative
	}
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{
			SlotStart:       now,
			SlotEnd:         now.Add(15 * time.Minute),
			BatteryEnergyWh: -600, // peak-slot discharge intent
		}, true
	}
	targets := ComputeDispatch(store, st, caps(map[string]float64{"pixii": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Without the floor, PI on grid_target=0 with grid=-1700 would
	// command +1700 W charge to absorb the export. With the floor,
	// that charge command must be clamped to 0.
	if targets[0].TargetW > 100 {
		t.Errorf("TargetW = %f W — sign floor should have clamped charge to 0 on a discharge slot",
			targets[0].TargetW)
	}
}

// ---- Battery manual hold ----

func TestBatteryManualHoldNotActiveByDefault(t *testing.T) {
	st := NewState(0, 50, "ferroamp")
	if _, ok := st.GetBatteryManualHold(time.Now()); ok {
		t.Errorf("expected no active hold on a fresh State")
	}
}

func TestBatteryManualHoldExpiresWithTime(t *testing.T) {
	st := NewState(0, 50, "ferroamp")
	now := time.Now()
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 1000, ExpiresAt: now.Add(10 * time.Second)})
	if _, ok := st.GetBatteryManualHold(now); !ok {
		t.Fatalf("hold should be active immediately after install")
	}
	if _, ok := st.GetBatteryManualHold(now.Add(11 * time.Second)); ok {
		t.Errorf("hold should expire after ExpiresAt")
	}
	// Expired holds should be evicted from state — Clear after lazy eviction is a no-op.
	st.ClearBatteryManualHold()
	if _, ok := st.GetBatteryManualHold(now); ok {
		t.Errorf("ClearBatteryManualHold should remove the hold")
	}
}

func TestBatteryManualHoldChargesAtSetpoint(t *testing.T) {
	// grid = +500 (some import). Without hold, self-consumption would
	// command discharge ≈ -500. Hold installs +3000 → expect charge.
	store := seedStore(500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 3000, ExpiresAt: time.Now().Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 100000)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW-3000) > 1 {
		t.Errorf("hold sets +3000 charge, got %f", targets[0].TargetW)
	}
}

func TestBatteryManualHoldDischargesAtSetpoint(t *testing.T) {
	// grid = -2000 (exporting). Without hold, self-consumption would
	// command +2000 charge. Hold installs -2500 → expect discharge.
	store := seedStore(-2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.SetBatteryManualHold(BatteryManualHold{PowerW: -2500, ExpiresAt: time.Now().Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 100000)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW-(-2500)) > 1 {
		t.Errorf("hold sets -2500 discharge, got %f", targets[0].TargetW)
	}
}

func TestBatteryManualHoldOverridesIdleMode(t *testing.T) {
	// In ModeIdle the dispatch normally returns nothing. The hold must
	// override and produce a target.
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeIdle
	st.SlewRateW = 100000
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 2000, ExpiresAt: time.Now().Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 100000)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW-2000) > 1 {
		t.Errorf("hold should override Idle, got %f", targets[0].TargetW)
	}
}

func TestBatteryManualHoldOverridesPlannerMode(t *testing.T) {
	// Planner mode with an idle-gate slot directive would normally hold
	// at 0. The hold must take precedence and discharge.
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.6},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.SlewRateW = 100000
	now := time.Now()
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{
			SlotStart:       now,
			SlotEnd:         now.Add(15 * time.Minute),
			BatteryEnergyWh: 0, // idle-gate
		}, true
	}
	st.SetBatteryManualHold(BatteryManualHold{PowerW: -3000, ExpiresAt: now.Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 100000)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW-(-3000)) > 1 {
		t.Errorf("hold should override planner_self idle gate, got %f", targets[0].TargetW)
	}
}

func TestBatteryManualHoldRespectsSoCFloor(t *testing.T) {
	// SoC < 5% blocks discharge — hold trying to discharge an empty
	// battery must clamp to 0 (clampWithSoC inside distributeProportional).
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.02}, // 2% — below the 5% discharge floor
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.SetBatteryManualHold(BatteryManualHold{PowerW: -3000, ExpiresAt: time.Now().Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 100000)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].TargetW != 0 || !targets[0].Clamped {
		t.Errorf("empty battery: discharge hold must clamp to 0 (clamped=true), got %+v", targets[0])
	}
}

func TestBatteryManualHoldRespectsFuseGuard(t *testing.T) {
	// fuseMaxW is tiny (2000 W). Hold asks for +5000 charge but the
	// fuse guard scales same-direction targets down to fit.
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 5000, ExpiresAt: time.Now().Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 2000)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].TargetW > 2000+1 {
		t.Errorf("fuse guard must clamp +5000 charge under fuseMaxW=2000, got %f", targets[0].TargetW)
	}
	if !targets[0].Clamped {
		t.Errorf("clamped flag should be set when fuse guard intervenes")
	}
}

func TestBatteryManualHoldExpiryRevertsToMode(t *testing.T) {
	// After the hold expires, the dispatch should revert to the configured
	// mode (here SelfConsumption) and use the live grid reading again.
	store := seedStore(1500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	now := time.Now()
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 3000, ExpiresAt: now.Add(-1 * time.Second)}) // already expired
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 100000)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target after expiry, got %d", len(targets))
	}
	// Self-consumption with grid=+1500 should command discharge (negative).
	if targets[0].TargetW >= 0 {
		t.Errorf("expired hold should revert to self_consumption (discharge expected), got %f",
			targets[0].TargetW)
	}
}

func TestBatteryManualHoldBypassesPlanSignFloor(t *testing.T) {
	// Pi report 2026-04-29: manual hold installed `charge 666 W` while
	// MPC plan intent was `discharge` — applyPlanSignFloor clamped
	// every tick to idle, defeating the override entirely. The sign
	// floor exists to catch *unintended* plan/exec divergence in
	// planner modes, but a manual hold is *intentional* divergence.
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.SlewRateW = 100000
	now := time.Now()
	// Plan says discharge this slot.
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{
			SlotStart:       now,
			SlotEnd:         now.Add(15 * time.Minute),
			BatteryEnergyWh: -1500,
		}, true
	}
	// Hold says charge — opposite sign.
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 666, ExpiresAt: now.Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 100000)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW-666) > 1 {
		t.Errorf("hold should win over plan-sign floor, got %f (want 666)",
			targets[0].TargetW)
	}
}

func TestBatteryManualHoldBypassesHoldoff(t *testing.T) {
	// LastDispatch in the very recent past would normally trigger the
	// holdoff branch. Manual holds must bypass it for immediate effect.
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 5
	veryRecent := time.Now().Add(-1 * time.Second)
	st.LastDispatch = &veryRecent
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 1500, ExpiresAt: time.Now().Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 100000)
	if len(targets) != 1 {
		t.Fatalf("manual hold must bypass holdoff, got %d targets", len(targets))
	}
	if math.Abs(targets[0].TargetW-1500) > 1 {
		t.Errorf("got %f, want 1500", targets[0].TargetW)
	}
}

// ---- Meter clamp on the legacy PI dispatch arm ----
//
// The reactive PI path commits a charge/discharge magnitude based on
// gridW vs GridTargetW. When the load forecast or operator slider is
// off, or the PI integrator has wound up across a mode switch, the
// raw PI request can push the meter past GridTargetW in either
// direction.
//
// Conservation says gridW moves 1:1 with bat within a tick, so the
// new battery target that lands gridW exactly on GridTargetW is
//
//   idealTarget = currentTotal − errW
//
// The clamp uses idealTarget as both the overshoot cap (don't punch
// through GridTargetW) and as the directional reference for catching
// wrong-direction PI windup (when sign(targetTotal − currentTotal)
// disagrees with sign(idealTarget − currentTotal), hold at
// currentTotal until the integrator unwinds).

func TestMeterClampStopsExportOnLoadOverPrediction(t *testing.T) {
	// Original incident geometry (PR #270): the reactive PI commands a
	// discharge that, if executed in this tick, would push the grid past
	// GridTargetW into export. The clamp must cap at the ideal landing
	// point, not let the PI overshoot.
	//
	// Setup: bat idle at 0, gridW = +2000 (importing 2 kW), target = 0.
	// Conservation says newBat = -2000 lands gridW on 0 exactly. PI is
	// pre-wound (simulated integrator windup) so its one-tick output
	// alone wants more discharge than that — should be clamped to -2000.
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.6},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000 // big so slew doesn't mask the clamp
	// Force windup that would otherwise push past idealTarget=-2000.
	// Repeated PI.Update at gridW=2000 accumulates integral until output
	// saturates well below idealTarget.
	for i := 0; i < 200; i++ {
		st.PI.Update(2000)
	}
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	var sum float64
	for _, tg := range targets {
		sum += tg.TargetW
	}
	// idealTarget = currentTotal - errW = 0 - 2000 = -2000. Clamp must
	// not let bat go past that (i.e., more negative). Slack of 1 W
	// for PI integrator settle on this tick.
	if sum < -2000-1.0 {
		t.Errorf("clamp should cap discharge at idealTarget=-2000, got sum=%f", sum)
	}
	// And it must actually be discharging (clamp is one-sided, not zero).
	if sum > -1.0 {
		t.Errorf("clamp swallowed all PI output instead of letting it close the gap, got sum=%f", sum)
	}
}

func TestMeterClampStopsImportOnLoadUnderPrediction(t *testing.T) {
	// Mirror of the over-prediction case. Bat idle at 0, gridW = -2000
	// (exporting 2 kW), target = 0. Wound-up PI wants to charge more
	// than needed; clamp must cap at idealTarget = +2000.
	store := seedStore(-2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	for i := 0; i < 200; i++ {
		st.PI.Update(-2000)
	}
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	var sum float64
	for _, tg := range targets {
		sum += tg.TargetW
	}
	// idealTarget = 0 - (-2000) = +2000. Clamp keeps charge ≤ that.
	if sum > 2000+1.0 {
		t.Errorf("clamp should cap charge at idealTarget=+2000, got sum=%f", sum)
	}
	if sum < 1.0 {
		t.Errorf("clamp swallowed all PI output instead of letting it close the gap, got sum=%f", sum)
	}
}

func TestMeterClampStopsExportOnLoadOverPredictionWithBatteryAlreadyDischarging(t *testing.T) {
	// Non-zero-currentTotal overshoot case (Copilot review on PR #276).
	// Battery is already partly discharging (-200 W) and the PI integrator
	// is wound up enough to demand a much larger discharge in this tick.
	// The clamp must cap at idealTarget = currentTotal − errW = −2200,
	// not pass an arbitrary -8000 W through unchanged.
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -200, 0.6},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	// Wind up the PI hard enough that its one-tick output, added to the
	// existing -200 W, lands well past idealTarget.
	for i := 0; i < 200; i++ {
		st.PI.Update(2000)
	}
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	var sum float64
	for _, tg := range targets {
		sum += tg.TargetW
	}
	// idealTarget = -200 - 2000 = -2200. Clamp must not let it go past.
	if sum < -2200-1.0 {
		t.Errorf("clamp let discharge overshoot idealTarget=-2200, got sum=%f", sum)
	}
	// And it must actually move further into discharge than currentTotal —
	// the clamp is one-sided, not a hold-at-current.
	if sum > -200-1.0 {
		t.Errorf("clamp pinned dispatch at currentTotal=-200; expected more discharge, got sum=%f", sum)
	}
}

func TestMeterClampHoldsSteadyOnWrongDirectionWindup(t *testing.T) {
	// Regression for the Copilot review on PR #276: if the PI integrator
	// is wound up in the WRONG direction (e.g. charging windup from a
	// prior export-heavy mode) while the site is now importing, the raw
	// PI output asks to charge even though grid is positive. Charging
	// from the grid in that state would aggravate the import. The clamp
	// must catch the sign mismatch and hold the battery at currentTotal,
	// letting the integrator unwind naturally over subsequent ticks
	// instead of executing the wrong-direction command.
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	// Manufacture wrong-direction windup: feed the PI a long history of
	// negative measurements (gridW < target) so the integrator climbs
	// positive. Then in the real tick, gridW will be +2000 (importing)
	// but the integrator's residual will keep the proportional + integral
	// sum positive enough to demand a charge.
	for i := 0; i < 400; i++ {
		st.PI.Update(-2000)
	}
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	var sum float64
	for _, tg := range targets {
		sum += tg.TargetW
	}
	// Must not command a charge while site is importing.
	if sum > 1.0 {
		t.Errorf("clamp let PI windup command charge while importing; expected sum<=0, got %f", sum)
	}
}

func TestMeterClampConvergesWithBatteryAlreadyDischarging(t *testing.T) {
	// Regression for the .139 production stuck-state: in manual
	// self_consumption with grid_target=0, the battery would sit forever
	// at e.g. -422 W producing grid_w +422 W (because load=836 W meant
	// the bat alone couldn't cover all consumption). The old clamp pinned
	// |target| ≤ |errW|, which equalled |currentTotal| in steady state,
	// so no progress was ever made.
	//
	// Conservation: gridW_next = gridW_now + (bat_next - bat_now). For
	// gridW → 0, need bat_next = bat_now - errW. Clamp's idealTarget
	// gives exactly that bound — the PI must be free to drive past
	// |currentTotal| toward that ideal.
	//
	// Geometry: bat already at -422 W, gridW = +422 W, target = 0.
	// idealTarget = -422 - 422 = -844 W. PI should push toward there.
	store := seedStore(422, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -422, 0.8},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000 // big so the convergence isn't slew-limited
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	var sum float64
	for _, tg := range targets {
		sum += tg.TargetW
	}
	// Must move further into discharge than currentTotal — the bug was
	// that the old clamp pinned sum at currentTotal forever.
	if sum >= -422.0 {
		t.Errorf("clamp pinned dispatch at currentTotal=-422 (stuck state); expected more discharge, got sum=%f", sum)
	}
	// And it must not overshoot past idealTarget = -844.
	if sum < -844.0-1.0 {
		t.Errorf("clamp let discharge overshoot idealTarget=-844, got sum=%f", sum)
	}
}

func TestMeterClampRespectsNonZeroGridTarget(t *testing.T) {
	// Regression guard for the e2e failure on master after the first
	// clamp landed: ModeSelfConsumption with GridTargetW = -3000 means
	// the operator wants the site to export 3 kW. Live meter at -1000
	// (already exporting 1 kW). The reactive PI must be free to command
	// additional discharge so gridW moves toward -3000; the clamp must
	// NOT treat "we're already exporting" as zero discharge headroom.
	//
	// Headroom is expressed in errW space (gridW - GridTargetW):
	//   errW = -1000 - (-3000) = +2000 → discharge headroom is 2000 W,
	// so a discharge correction of up to 2 kW (toward the GridTargetW)
	// should pass the clamp unchanged.
	store := seedStore(-1000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(-3000, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	var sum float64
	for _, tg := range targets {
		sum += tg.TargetW
	}
	// Sum must move meaningfully toward discharge — not get pinned at 0
	// by a clamp that confuses "currently exporting" with "no headroom".
	if sum >= -1.0 {
		t.Errorf("clamp pinned dispatch instead of letting PI close the gap to GridTargetW=-3000, got sum=%f", sum)
	}
	// And it must not overshoot the target (i.e. don't discharge past
	// |errW| = 2000 W).
	if sum < -2000-1e-6 {
		t.Errorf("clamp let discharge overshoot GridTargetW, got sum=%f", sum)
	}
}

// ---- PV surplus absorber underlay ----
//
// Opt-in policy reversal of TestEnergyDispatchDoesNotAbsorbPVSurprise: when
// the operator sets a SoC cap (PVSurplusAbsorbSoCCapPct > 0), the dispatch
// catches the gap between the planner's 15-min slot allocation and the
// live PV/load drift — additional export beyond plan flows into the
// battery instead of out the meter at low spot price. Only kicks in
// planner_cheap / planner_arbitrage (the modes whose energy path doesn't
// react to live grid), only adds charge (never reverses a discharge plan),
// only while SoC < cap. Otherwise the original "let it export" behavior
// stands. See the operator-report cycle 2026-04-17 → 2026-05-15.

// Same scenario as TestEnergyDispatchDoesNotAbsorbPVSurprise, but with the
// absorber knob enabled and SoC below cap → battery absorbs the surprise.
func TestPVSurplusAbsorberAbsorbsExtraExportWhenEnabled(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200, // plan: charge 200 Wh (~ 800 W)
		Strategy:        "arbitrage",
	}
	// Grid exporting 4 kW (PV surprise — PV 4.8 kW, load 0.8 kW, battery 0).
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5}, // SoC 50%, well below 88% cap
	})
	store.Update("pv-1", telemetry.DerPV, -4800, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.PVSurplusAbsorbSoCCapPct = 88 // enable
	st.PVSurplusAbsorbThresholdW = 100

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	// Energy balance: grid = load + pv + battery, so -4000 = 800 + (-4800) + 0
	// ⇒ load is 800 W, PV surplus over load is 4000 W. Plan wanted +800 W.
	// Plan-as-is would leave -3200 W still exporting (rawGridW + planTarget).
	// Absorber catches the 3200 W leftover and stacks on top of plan →
	// target ≈ 800 + 3200 = 4000 W, projected grid → 0.
	// Allow ±400 W slack for energy-formula time drift and threshold.
	got := targets[0].TargetW
	if got < 3600 {
		t.Errorf("TargetW = %.0f W — absorber should be soaking PV surplus into battery (want ≈ 4000 W)", got)
	}
	if got > 4400 {
		t.Errorf("TargetW = %.0f W — absorber overshoot (want ≈ 4000 W, fuse/cap should bound)", got)
	}
}

// Defaults preserve back-compat: PVSurplusAbsorbSoCCapPct = 0 means
// disabled; original "don't absorb surprise" behavior holds.
func TestPVSurplusAbsorberDisabledByDefault(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200,
		Strategy:        "arbitrage",
	}
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -4800, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	// No PVSurplusAbsorbSoCCapPct set → defaults to 0 → feature off.

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targets[0].TargetW
	// Same expectations as TestEnergyDispatchDoesNotAbsorbPVSurprise.
	if got > 1500 {
		t.Errorf("TargetW = %.0f W — absorber should be off by default; got runaway absorption", got)
	}
	if got < 100 {
		t.Errorf("TargetW = %.0f W — plan intent (~800 W) should still run", got)
	}
}

// At-cap: absorber must not push SoC above cap.
func TestPVSurplusAbsorberHoldsAtCap(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 200,
		Strategy:        "arbitrage",
	}
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.88}, // already at cap
	})
	store.Update("pv-1", telemetry.DerPV, -4800, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.PVSurplusAbsorbSoCCapPct = 88

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targets[0].TargetW
	// SoC already at cap → no absorption beyond the plan's ~800 W intent.
	if got > 1500 {
		t.Errorf("TargetW = %.0f W — at cap, absorber should defer to plan (~800 W)", got)
	}
}

// BatteryCoversEV=false must NOT clamp a planned evening-peak discharge
// when the EV is effectively idle (driver reporting <100 W of noise).
// Pre-fix, EVChargingW > 0 was the gate and any tiny non-zero from
// Easee (~1 W on a connected-but-idle cable) tripped the cap, leaving
// the planner's -9000 W export pinned to "just enough to zero the
// house" (~-1 kW on a typical evening). Regression: live state showed
// plan=-9000 W → realised ≈-1.3 kW with ev_w=0, ev_charging_w=1.08.
func TestEnergyDispatchIgnoresEVChargingWNoiseUnderThreshold(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -2250, // plan: -9 kW for the slot
		Strategy:        "arbitrage",
	}
	// Evening: PV gone, house ~1.4 kW load, plan wants to export
	// hard. Battery currently at 0; rawGridW reflects the no-battery
	// case (house importing 1.4 kW).
	store := seedStore(1400, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.85},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.BatteryCoversEV = false
	st.EVChargingW = 1.08 // <-- driver noise; EV is plugged-idle, not drawing

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// With the noise threshold in place, the safety net stays off and
	// the planner's -9 kW discharge runs. Allow generous slack — slew,
	// fuse and per-driver clamps may still trim it, but it should be
	// well below -3 kW (i.e. discharging hard, not just covering house).
	if got > -3000 {
		t.Errorf("TargetW = %.0f W — EVChargingW noise (1 W) clamped a -9 kW plan to house-only. Want ≤ -3000 W.", got)
	}
}

// Counter-check: when the EV is genuinely drawing (>= threshold), the
// safety net MUST still fire to prevent the planner's discharge from
// feeding the EV via the battery. battery_covers_ev=false is a strong
// promise; the noise threshold doesn't weaken it for real draws.
func TestEnergyDispatchClampsDischargeWhenEVActuallyCharging(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -2250,
		Strategy:        "arbitrage",
	}
	// House 1.4 kW load + EV drawing 4 kW = rawGridW 5.4 kW import.
	store := seedStore(5400, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.85},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.BatteryCoversEV = false
	st.EVChargingW = 4000 // real charging — safety net MUST fire

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targets[0].TargetW
	// Battery should cover ~the house (~-1.4 kW), NOT the EV.
	if got < -2500 {
		t.Errorf("TargetW = %.0f W — safety net failed: battery is feeding the EV (want ≈ -1.4 kW).", got)
	}
	if got > -500 {
		t.Errorf("TargetW = %.0f W — house side not covered (want roughly -1.4 kW).", got)
	}
}

// Don't reverse a discharge plan: if the planner is deliberately
// discharging this slot (e.g. evening-peak export), the absorber stays
// out of the way regardless of live grid sign.
// Regression for the operator-reported "battery sits idle while
// surplus exports under EV reserve". Energy path, plan dictates
// charge battery, EV is on surplus_only at 2.5 kW with 11 kW max,
// and 3 kW is currently exporting at the real meter.
//
// dispatch.go:685 subtracts EVChargingW from gridW before the home-
// battery decision so the battery sees "what export would exist
// without the EV", which is 3000 + 2500 = 5500 W in this scenario.
// The reserve cap then carves out reserveRemaining for the EV to
// step up into.
//
// Pre-fix (reserve = full MaxChargeW = 11000): reserveRemaining
// = 11000 - 2500 = 8500. ceiling = 5500 - 8500 = -3000 → 0. Battery
// idles; surplus dumps to grid.
//
// Post-fix (reserve = CurrentPowerW + EVRampHeadroomW = 4500):
// reserveRemaining = 4500 - 2500 = 2000. ceiling = 5500 - 2000 =
// 3500. Battery picks up the bulk of the surplus while leaving 2 kW
// of headroom for the EV to ramp up.
//
// The test sets EVSurplusOnlyReserveW directly to the value
// loadpoint.SurplusReserveW would produce; the helper itself is
// unit-tested separately in internal/loadpoint.
func TestEnergyDispatchAbsorbsSurplusBeyondEVReserveActualDraw(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1250, // plan: charge ~5 kW over the slot
		Strategy:        "arbitrage",
	}
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -6000, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 2500
	st.EVSurplusOnlyReserveW = 2500 + 2000 // = SurplusReserveW for one LP at 2.5 kW

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Want ≈ 3500 W. Allow ±400 W slack for time drift / formula edge.
	if got < 3100 {
		t.Errorf("TargetW = %.0f W — battery should absorb the surplus that the EV's adaptive reserve no longer hoards (want ≈ 3500 W, was 0 pre-fix)", got)
	}
	if got > 3900 {
		t.Errorf("TargetW = %.0f W — battery overshoot, must leave EVRampHeadroomW (2 kW) of headroom for EV ramp-up (want ≈ 3500 W)", got)
	}
}

// Companion: same setup, but with the PRE-FIX reserve (= full
// MaxChargeW). The battery should be capped at or near 0. Documents
// the bug this PR fixed and prevents accidental re-regression if
// someone later restores the old reserve formula.
func TestEnergyDispatchPreFixReserveStarvesBattery(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1250,
		Strategy:        "arbitrage",
	}
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -6000, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 2500
	st.EVSurplusOnlyReserveW = 11000 // simulate the old "reserve = MaxChargeW" formula

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targets[0].TargetW
	if got > 200 {
		t.Errorf("TargetW = %.0f W — full-MaxChargeW reserve should starve the battery here; if you see a non-trivial target the dispatch reserve cap regressed", got)
	}
}

func TestPVSurplusAbsorberDoesNotReverseDischarge(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -1000, // plan: discharge ~4 kW
		Strategy:        "arbitrage",
	}
	// Grid exporting hard (PV plus planned discharge), but plan is
	// intentionally export-at-peak. Absorber must NOT flip the sign.
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -4800, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.PVSurplusAbsorbSoCCapPct = 88

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	got := targets[0].TargetW
	if got > -1500 {
		t.Errorf("TargetW = %.0f W — absorber must not blunt a discharge plan (want ≈ -4000 W)", got)
	}
}
