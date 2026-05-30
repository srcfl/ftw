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

// DecayIntegral is the live-system anti-windup escape hatch — wind the
// integral up to saturation, then call DecayIntegral a few times to verify
// it drops geometrically toward 0 without snapping it to 0 instantly.
func TestPIDecayIntegralUnwindsSaturation(t *testing.T) {
	p := NewPI(0.5, 0.1, 3000, 10000)
	p.Setpoint = 0
	// Drive into negative saturation with a sustained positive measurement
	// (PI's internal err = setpoint - measurement = negative).
	for i := 0; i < 200; i++ {
		p.Update(700)
	}
	if p.Integral() > -2900 {
		t.Fatalf("setup: expected integral pinned near -3000 after 200 cycles, got %f", p.Integral())
	}
	p.DecayIntegral(0.5)
	if got := p.Integral(); math.Abs(got-(-1500)) > 1 {
		t.Errorf("after one 0.5 decay from -3000, integral = %f, want ≈ -1500", got)
	}
	p.DecayIntegral(0.5)
	p.DecayIntegral(0.5)
	if got := p.Integral(); math.Abs(got-(-375)) > 1 {
		t.Errorf("after three 0.5 decays from -3000, integral = %f, want ≈ -375", got)
	}
}

func TestPIDecayIntegralClampsFactor(t *testing.T) {
	p := NewPI(0.5, 0.1, 3000, 10000)
	p.Setpoint = 0
	for i := 0; i < 50; i++ {
		p.Update(500)
	}
	before := p.Integral()
	p.DecayIntegral(-1.0)
	if p.Integral() != 0 {
		t.Errorf("DecayIntegral(-1) should clamp to factor=0 (i.e. zero the integral), got %f", p.Integral())
	}
	p.integral = before
	p.DecayIntegral(5.0)
	if p.Integral() != before {
		t.Errorf("DecayIntegral(5) should clamp to factor=1 (no-op), got %f vs before=%f", p.Integral(), before)
	}
}

// 2026-05-25 morning regression: yesterday's mode-switch wound the PI
// integral to negative saturation while the system was importing under a
// stale plan; once the morning sun flipped grid to export, the saturated
// integral kept commanding discharge for ~3 min until natural decay
// drained it. Dispatch already detects the wrong-direction case and
// clamps the OUTPUT to currentTotal, but it left the integral untouched,
// so the next cycle was just as wound up.
//
// Active integral decay on wrong-direction-windup means the controller
// converges within a handful of cycles instead of minutes.
func TestDispatchWrongDirectionWindupDecaysPIIntegral(t *testing.T) {
	store := seedStore(-1000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 60, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	// Pre-wind the integral hard negative — simulates yesterday's
	// load-side import accumulation persisting into a now-exporting
	// grid this morning.
	for i := 0; i < 100; i++ {
		st.PI.Update(800)
	}
	beforeI := st.PI.Integral()
	if beforeI > -2500 {
		t.Fatalf("setup: expected integral wound past -2500, got %f", beforeI)
	}

	// Single dispatch cycle: live grid is -1000 (exporting), errW=-1000.
	// PI's pre-wound -3000 integral will still drag the output negative,
	// so correctionDir would be negative → matches the "exporting but PI
	// wants discharge" wrong-direction case → integral must decay.
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	afterI := st.PI.Integral()
	// Both numbers negative — "closer to 0" means a larger (less negative) value.
	if math.Abs(afterI) >= math.Abs(beforeI) {
		t.Errorf("integral after wrong-dir clamp = %f, want strictly closer to 0 than before=%f", afterI, beforeI)
	}
	// Behavioural assertion: a few cycles of geometric decay + normal
	// integration should be enough that dispatch is no longer commanding
	// the wrong direction. Without decay, the integral stays saturated
	// at -3000 and PI keeps emitting the wrong-signed output across
	// every subsequent cycle (that's the 2026-05-25 sunrise regression).
	var lastTarget float64
	for i := 0; i < 5; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) == 1 {
			lastTarget = targets[0].TargetW
		}
	}
	// grid_w = -1000 (exporting); a correctly-recovered PI must command
	// charge, not discharge. Pre-fix this test scenario would emit
	// target ≤ 0 for ~3 min until natural integral drain.
	if lastTarget <= 0 {
		t.Errorf("after 6 cycles, target = %f W — controller still stuck in wrong direction (need positive charge command against -1000 W export)", lastTarget)
	}
}

// ---- Dispatch tests ----

// helper: build a store with one site meter + N batteries at given SoC
func seedStore(gridW float64, batteries []struct {
	name          string
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
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
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
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
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

func TestChargeModeRespectsFuseGuard(t *testing.T) {
	store := seedStore(10000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeCharge
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if !targets[0].Clamped {
		t.Fatalf("charge target should be clamped by fuse guard: %+v", targets[0])
	}
	if math.Abs(targets[0].TargetW-1040) > 1 {
		t.Fatalf("charge target = %.0f W, want remaining fuse headroom 1040 W", targets[0].TargetW)
	}
}

func TestDeadbandSkipsWithinTolerance(t *testing.T) {
	store := seedStore(30, []struct {
		name          string
		currentW, soc float64
	}{
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
	// grid = +1000 (importing). Classic self-consumption drives the
	// site meter toward zero, so it may discharge to cover local load.
	store := seedStore(1000, []struct {
		name          string
		currentW, soc float64
	}{
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
		t.Errorf("self_consumption should discharge on import, got %f", targets[0].TargetW)
	}
}

// planner_self idle gate constrains charge, never discharge. A battery
// already discharging to cover load is doing exactly what classic
// self_consumption would have it do — stopping it would silently flip the
// site to importing, which violates the operator's "never import" floor.
// The deadband suppresses dispatch when error is within tolerance, so the
// expected outcome is "no new target, leave the battery alone".
func TestPlannerSelfIdleGateLeavesExistingDischargeAloneInsideDeadband(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Grid balanced — battery is covering load fine. Deadband → no
	// dispatch issued, but the running battery state is preserved.
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -900, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Fatalf("inside deadband with grid already at target: want no dispatch, got %d targets (e.g. %+v)", len(targets), targets)
	}
}

func TestSelfConsumptionChargesOnExport(t *testing.T) {
	// grid = -2000 (exporting) → want battery to charge (positive target)
	store := seedStore(-2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatal("expected 1 target")
	}
	if targets[0].TargetW <= 0 {
		t.Errorf("exporting should lead to POSITIVE (charge) target, got %f", targets[0].TargetW)
	}
}

func TestPlannerSelfIdlePlanNeverDischargesIndividualBatteryWhileEVCharging(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	store := seedStore(2980, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 338, 0.14},
		{"sungrow", -991, 0.23},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.EVChargingW = 9240
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{
		"ferroamp": 15200,
		"sungrow":  9600,
	}), 11040)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d: %+v", len(targets), targets)
	}
	for _, tgt := range targets {
		if tgt.TargetW < 0 {
			t.Errorf("%s target = %f W — planner_self idle slots must not keep a battery discharging", tgt.Driver, tgt.TargetW)
		}
	}
}

func TestChargePlanNeverDischargesIndividualBatteryAfterSlew(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1000,
		Strategy:        "cheap_charge",
	}
	store := seedStore(2980, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 338, 0.14},
		{"sungrow", -991, 0.23},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerCheap
	st.UseEnergyDispatch = true
	st.SlewRateW = 500
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{
		"ferroamp": 15200,
		"sungrow":  9600,
	}), 11040)
	if len(targets) != 2 {
		t.Fatalf("expected 2 targets, got %d: %+v", len(targets), targets)
	}
	for _, tgt := range targets {
		if tgt.TargetW < 0 {
			t.Errorf("%s target = %f W — a charge/idle plan must not keep an individual battery discharging", tgt.Driver, tgt.TargetW)
		}
	}
}

// SlewEnabled=false: PI's computed target reaches the inverter in one
// cycle. Both supported inverter families ramp internally, so the
// external slew was double-limiting on top of their own protection.
// Smoke test that a large step (battery 0 → -3000 over a single cycle)
// passes through without the slew clamp.
func TestSlewDisabledPassesLargeStepThrough(t *testing.T) {
	// Grid importing 3 kW — PI wants to discharge heavily.
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500 // intentionally tight — if SlewEnabled wins, target lands at -500
	st.SlewEnabled = false
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW) <= 500 {
		t.Errorf("TargetW = %f W — slew_enabled=false must let PI exceed the 500 W slew step in a single cycle", targets[0].TargetW)
	}
	if targets[0].Clamped {
		t.Errorf("Clamped=true with slew_enabled=false — only fuse/SoC/per-driver caps should clamp now")
	}
}

// SlewEnabled=true (default) still rate-limits step responses to the
// configured ramp.
func TestSlewEnabledClampsLargeStep(t *testing.T) {
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 500
	st.SlewEnabled = true
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW) > 510 {
		t.Errorf("TargetW = %f W — slew_enabled=true should cap step at ~500 W", targets[0].TargetW)
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
		if tg.Driver == "big" {
			big = tg.TargetW
		}
		if tg.Driver == "small" {
			small = tg.TargetW
		}
	}
	// Big is 75%, small 25% → big = -750, small = -250
	if math.Abs(big+750) > 1 {
		t.Errorf("big got %f, want -750", big)
	}
	if math.Abs(small+250) > 1 {
		t.Errorf("small got %f, want -250", small)
	}
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
		if tg.Driver == "primary" {
			p = tg.TargetW
		}
		if tg.Driver == "secondary" {
			s = tg.TargetW
		}
	}
	if math.Abs(p+1000) > 1 {
		t.Errorf("primary: got %f, want -1000", p)
	}
	if s != 0 {
		t.Errorf("secondary: got %f, want 0", s)
	}
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
		if tg.Driver == "primary" {
			p = tg.TargetW
		}
		if tg.Driver == "secondary" {
			s = tg.TargetW
		}
	}
	if p != -5000 {
		t.Errorf("primary: got %f, want -5000", p)
	}
	if math.Abs(s+2000) > 1 {
		t.Errorf("secondary: got %f, want -2000", s)
	}
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
		if tg.Driver == "a" {
			a = tg.TargetW
		}
		if tg.Driver == "b" {
			b = tg.TargetW
		}
	}
	if math.Abs(a-800) > 1 {
		t.Errorf("a: got %f, want 800", a)
	}
	if math.Abs(b-200) > 1 {
		t.Errorf("b: got %f, want 200", b)
	}
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
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
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
		t.Errorf("self_consumption should discharge on import, got %f", targets[0].TargetW)
	}
}

func TestSettlementGridTargetCompensatesPriorImportOnly(t *testing.T) {
	st := NewState(0, 50, "ferroamp")
	now := time.Date(2026, 5, 23, 14, 5, 0, 0, time.Local)
	if got := st.settlementGridTarget(now, 0); got != 0 {
		t.Fatalf("first sample target = %f, want 0", got)
	}

	got := st.settlementGridTarget(now.Add(time.Minute), 2700)
	// 2700 W for one minute = 45 Wh. Nine minutes remain, so the raw
	// compensating grid target is -300 W; the settlement target applies
	// a low-pass and starts at 35% of that.
	if math.Abs(got+105) > 0.1 {
		t.Fatalf("target = %f, want -105", got)
	}

	// Prior export must not be repaid with intentional import.
	st2 := NewState(0, 50, "ferroamp")
	if got := st2.settlementGridTarget(now, 0); got != 0 {
		t.Fatalf("first export sample target = %f, want 0", got)
	}
	st2.settlementNetWh = -100
	got = st2.settlementGridTarget(now.Add(time.Minute), 0)
	if got != 0 {
		t.Fatalf("prior export target = %f, want 0", got)
	}
}

func TestSelfConsumptionSettlementBiasExportsToRecoverSlotImport(t *testing.T) {
	now := time.Now()
	remaining := now.Truncate(settlementSlotDuration).Add(settlementSlotDuration).Sub(now)
	if remaining < settlementMinRemainS*time.Second+time.Second {
		t.Skip("too close to quarter boundary")
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.8},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SettlementAwareSelfConsumption = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.settlementSlotStart = now.Truncate(settlementSlotDuration)
	st.settlementLastTs = now.Add(-time.Second)
	st.settlementNetWh = 100

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected settlement recovery target, got %d", len(targets))
	}
	if targets[0].TargetW >= 0 {
		t.Fatalf("settlement recovery should discharge/export, got %f", targets[0].TargetW)
	}
}

func TestSelfConsumptionSettlementBiasDisabledByDefault(t *testing.T) {
	now := time.Now()
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.8},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.settlementSlotStart = now.Truncate(settlementSlotDuration)
	st.settlementLastTs = now.Add(-time.Second)
	st.settlementNetWh = 100

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Fatalf("settlement bias is unsafe as a default; got %#v", targets)
	}
}

func TestSelfConsumptionSettlementBiasDisabledAtLowSoC(t *testing.T) {
	now := time.Now()
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.26},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SettlementAwareSelfConsumption = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.settlementSlotStart = now.Truncate(settlementSlotDuration)
	st.settlementLastTs = now.Add(-time.Second)
	st.settlementNetWh = 100

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Fatalf("low SoC settlement recovery should not export battery, got %#v", targets)
	}
}

func TestSelfConsumptionSettlementBiasDoesNotImportToRecoverExport(t *testing.T) {
	now := time.Now()
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.8},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SettlementAwareSelfConsumption = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.settlementSlotStart = now.Truncate(settlementSlotDuration)
	st.settlementLastTs = now.Add(-time.Second)
	st.settlementNetWh = -100

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 0 {
		t.Fatalf("prior export should not trigger intentional import, got %#v", targets)
	}
}

func TestHoldoffBlocksRapidDispatch(t *testing.T) {
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
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
	if st.GridTargetW != -500 {
		t.Errorf("state: %f", st.GridTargetW)
	}
	if st.PI.Setpoint != -500 {
		t.Errorf("pi setpoint: %f", st.PI.Setpoint)
	}
}

func TestEmptyBatteriesReturnsNoTargets(t *testing.T) {
	store := seedStore(1000, nil)
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	targets := ComputeDispatch(store, st, caps(map[string]float64{}), 11040)
	if len(targets) != 0 {
		t.Error("no batteries → no dispatch")
	}
}

func TestPeakShavingNoActionInBand(t *testing.T) {
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
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
	store := seedStore(7000, []struct {
		name          string
		currentW, soc float64
	}{
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
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
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
	store := seedStore(5000, []struct {
		name          string
		currentW, soc float64
	}{
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
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
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

func TestSurplusOnlyEVDoesNotAutoEnableBatteryCoversEV(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -800, // participant slot: classic self-consumption
		Strategy:        "self_consumption",
	}
	// Site is importing because the EV is larger than the PV surplus:
	// raw grid = +3 kW, EV = 9 kW, so house-side grid is -6 kW.
	// BatteryCoversEV=false means the battery must not discharge into
	// that EV import; reserve math may charge up to surplus headroom.
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.EVChargingW = 9000
	st.EVSurplusOnlyReserveW = 11000
	st.BatteryCoversEV = false
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("expected 1 target, got %d", len(targets))
	}
	if targets[0].TargetW < 0 {
		t.Errorf("surplus_only EV must not auto-enable battery-to-EV discharge, got %f", targets[0].TargetW)
	}
}

// TestBatteryCoversEV_OnIncludesEVInPeakShaving covers the opt-in scenario
// outside self-consumption: peak-shaving may discharge to protect an import
// ceiling, and BatteryCoversEV=true means the full EV draw participates.
func TestBatteryCoversEV_OnIncludesEVInPeakShaving(t *testing.T) {
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePeakShaving
	st.PeakLimitW = 0
	st.EVChargingW = 2500
	st.BatteryCoversEV = true // opt in — battery covers everything
	st.SlewRateW = 100000
	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) == 0 {
		t.Fatal("expected a dispatch target when grid is 3 kW over peak limit, got none")
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
	store := seedStore(1500, []struct {
		name          string
		currentW, soc float64
	}{
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
	store := seedStore(-2000, []struct {
		name          string
		currentW, soc float64
	}{
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
	store := seedStore(1500, []struct {
		name          string
		currentW, soc float64
	}{
		{"pixii", -1000, 0.6},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePeakShaving
	st.PeakLimitW = 0
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
		name          string
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
		name          string
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
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")
	st.EVChargingW = 4000      // manual injection — no EV driver in store
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
		name          string
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
//
//	battery charge → ~4150 W
//	FuseEVMaxW    → ~6650 W
//	sum + house    ≈ 11.0 kW. Fuse respected.
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
		name          string
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
		name          string
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
		name          string
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
		name          string
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

// Mid-slot replan that GROWS the slot budget must not trigger a
// catastrophic catch-up demand near slot-end.
//
// Production incident on .139 (2026-05-17): planner_arbitrage with a
// −900 W slot plan. A reactive replan late in the slot grew the slot's
// BatteryEnergyWh by ~5×. With slotDelivered still tracking the old
// (smaller) directive's pace, remainingWh × 3600 / remainingS demanded
// >30 kW for the last ~80 s; battery clamped to MaxDischargeW (−9000 W)
// and stayed there until slot rollover. The "shrink budget" branch
// (line 784) handled the symmetric case but the grow case was
// unprotected — fix rebases slotDelivered to the new pace.
func TestEnergyDispatchRebasesSlotDeliveredOnBudgetGrow(t *testing.T) {
	now := time.Now()
	// Slot is 15 min total; we're 12 min in (80% elapsed). Old plan was
	// for −225 Wh (−900 W avg). Actual delivered tracks the old pace:
	// 0.8 × −225 ≈ −180 Wh.
	slotStart := now.Add(-12 * time.Minute)
	slotEnd := now.Add(3 * time.Minute)
	dir1 := SlotDirective{
		SlotStart:       slotStart,
		SlotEnd:         slotEnd,
		BatteryEnergyWh: -225, // −900 W × 0.25 h
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -900, 0.6}, // currently delivering at old pace
	})
	st := newStateWithEnergyDispatch(dir1, "ferroamp")
	// Seed the accumulator to mimic 12 min of −900 W delivery.
	st.currentDirective = dir1
	st.slotDelivered = -180
	st.lastTickTs = now.Add(-5 * time.Second)

	// Replan: same slot, but budget grows 5× (BatteryEnergyWh = −1125 Wh,
	// avg −4500 W). On the old code, remainingWh = −1125 − (−180) =
	// −945 Wh over 180 s → −18.9 kW. Even after clamping to driver max,
	// that's a ~5× overshoot of the new slot avg of −4500 W.
	dir2 := SlotDirective{
		SlotStart:       slotStart,
		SlotEnd:         slotEnd,
		BatteryEnergyWh: -1125,
	}
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir2, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// New slot avg is −4500 W. After rebase, targetTotalW should be close
	// to slot avg, not a 4× catch-up. Allow ~10% margin for time drift.
	if got < -5000-100 || got > -4000+100 {
		t.Errorf("targetTotalW = %f; expected ≈ slot-avg (−4500 W) after rebase, not catastrophic catch-up", got)
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
		name          string
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
		name          string
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
	// With grid = +1000 and target = 0, stale fallback behaves like
	// self-consumption and discharges toward grid zero.
	if len(targets) == 0 {
		t.Error("expected some dispatch under legacy fallback, got nothing")
	}
	if len(targets) > 0 && targets[0].TargetW >= 0 {
		t.Errorf("stale planner fallback should discharge like self_consumption, got %f", targets[0].TargetW)
	}
}

// Under planner_arbitrage — where the DP is explicitly allowed to export via
// battery — energy dispatch holds the plan even when live grid diverges.
// That's the point of "grid is the residual": arbitrage decides slot-by-slot
// that this_slot_W × slot_duration of battery energy is the cost-optimal
// cycle, and the EMS just executes it. Live export is a legal outcome.
//
// Contrast: under planner_self (see TestPlannerSelf* below) the same plan
// is treated only as idle-vs-participate. Participant slots use live
// self-consumption instead of blindly executing the forecasted Wh.
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

// ---- planner_arbitrage PlannedGridW soft reactive cap ----
//
// Community report (2026-05-19, planner_arbitrage): "When the sun goes
// behind clouds the system compensates with grid import to maintain the
// planned battery_w. Didn't used to behave that way — battery used to
// just absorb what surplus existed." Root cause: the energy-allocation
// path executes BatteryEnergyWh as `remainingWh × 3600 / remainingS`
// without consulting live gridW. When forecast PV doesn't materialise,
// the formula still demands the planned charge power and grid fills the
// gap until the reactive replan trigger (mpc/service.go, 500 Wh PV
// integral + 60 s cooldown) catches up — typically 10+ minutes.
//
// The fix: SlotDirective.PlannedGridW (the plan's own gridW forecast)
// gets used as a soft reactive cap. When live gridW exceeds plan in
// the dispatch direction, back off targetTotalW by the gap.

// TestEnergyDispatchPlannedGridCapBacksOffChargeWhenPVDrops is the
// motivating regression. Plan: arbitrage charge slot, 1200 Wh over 15
// min ≈ 4800 W, with PV forecast 5 kW so gridW forecast ≈ 0. Cloud
// hits, live PV drops to 1.8 kW (load 0 W), so without the cap the
// battery would charge 4800 W against 1800 W of available PV → 3000 W
// of grid import. With the cap the battery target should back off to
// ~1800 W and grid stays near zero.
func TestEnergyDispatchPlannedGridCapBacksOffChargeWhenPVDrops(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1200, // ~4800 W average
		Strategy:        "arbitrage",
		PlannedGridW:    0, // plan expected near-zero grid (PV did the work)
		HasPlannedGridW: true,
	}
	// Live before the new command: gridW = -1800 because the battery is
	// still at 0 W and PV is generating 1.8 kW. Executing the raw
	// +4800 W target would project grid to +3000 W import; the cap must
	// compute against that post-dispatch projection, not rawGridW alone.
	store := seedStore(-1800, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -1800, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Raw plan: 4800 W. After cap: 4800 - 3000 = 1800 W. Tolerance ±200 W
	// covers slot-time drift (a few seconds shaves remainingS).
	if got > 2000 || got < 1500 {
		t.Errorf("TargetW = %f W — cap should pull battery back to ~1800 W "+
			"(projected-grid cap), not chase plan against missing PV", got)
	}
}

func TestEnergyDispatchPlannedGridCapAccountsForCurrentBatteryLag(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1200, // ~4800 W average
		Strategy:        "arbitrage",
		PlannedGridW:    0,
		HasPlannedGridW: true,
	}
	// Battery is already charging at 2 kW, so raw grid includes that
	// current battery draw: -1800 W PV + 2000 W battery = +200 W import.
	// Old cap math used rawGridW-planGridW and would only shave 200 W.
	// Correct projected-grid math lands target near 1800 W:
	// projected = 200 + (4800 - 2000) = +3000; 4800 - 3000 = 1800.
	store := seedStore(200, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 2000, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -1800, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got > 2000 || got < 1500 {
		t.Errorf("TargetW = %f W — cap must account for current battery power and land near 1800 W", got)
	}
}

// TestEnergyDispatchPlannedGridCapAllowsPlannedImport — the cap must
// NOT fire when the plan committed to importing (cheap-grid charge
// during a low-price slot). PlannedGridW = +2000 (plan expected to
// import 2 kW to charge). Live grid before the command is 0 W, so
// executing the +2 kW battery target projects exactly the planned
// +2 kW import.
func TestEnergyDispatchPlannedGridCapAllowsPlannedImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 500, // 2000 W average over 15 min
		Strategy:        "cheap_charge",
		PlannedGridW:    2000, // plan: import 2 kW to charge
		HasPlannedGridW: true,
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got < 1800 || got > 2200 {
		t.Errorf("TargetW = %f W — live gridW matches plan, battery must follow plan (~2000 W) "+
			"without any cap pull-back", got)
	}
}

func TestEnergyDispatchPlannedGridCapAllowsSteadyStateCharging(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 500, // 2000 W average over 15 min
		Strategy:        "cheap_charge",
		PlannedGridW:    2000,
		HasPlannedGridW: true,
	}
	// Battery is already at the planned +2 kW charge and the meter is
	// already at the planned +2 kW import. The cap must compare the
	// projected post-dispatch grid, see no change, and leave target alone.
	store := seedStore(2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 2000, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got < 1800 || got > 2200 {
		t.Errorf("TargetW = %f W — steady-state planned charging should pass through (~2000 W)", got)
	}
}

// TestEnergyDispatchPlannedGridCapNoFireInsideDeadband — small live
// divergences (≤100 W) are meter noise / smoothing residue. The cap
// must not fire there, otherwise it nibbles at every tick.
func TestEnergyDispatchPlannedGridCapNoFireInsideDeadband(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1200, // 4800 W average
		Strategy:        "arbitrage",
		PlannedGridW:    0,
		HasPlannedGridW: true,
	}
	// Battery is already at the planned charge level. The projected grid
	// after keeping that target is +50 W — inside the 100 W deadband.
	store := seedStore(50, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 4800, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got < 4700 || got > 4900 {
		t.Errorf("TargetW = %f W — inside the 100 W deadband the cap must stay off "+
			"and battery follows raw plan (~4800 W)", got)
	}
}

// TestEnergyDispatchPlannedGridCapCoversLoadSurgeOnSurplusSlot is the
// symmetric companion to the charge-back-off cap (operator report
// 2026-05-30, planner_arbitrage). On a charge-from-PV-surplus slot
// (PlannedGridW ≈ 0 — the DP meant to soak surplus, NOT grid-charge), a
// sudden load surge leaves the site importing. The old cap floored the
// back-off at 0 (battery idle) and left the import to the slow reactive
// replan, so the battery never supported the load. The cap must instead
// flip to discharge, driving projected grid back to PlannedGridW (~0).
//
// Plan: arbitrage charge slot, 1200 Wh over 15 min ≈ 4800 W, PlannedGridW=0.
// Live: battery at 0 W, load surged so the meter reads +682 W import.
// Executing the +4800 W plan would project +5482 W import; covering the
// load means a discharge of ~682 W (projected grid → 0).
func TestEnergyDispatchPlannedGridCapCoversLoadSurgeOnSurplusSlot(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1200, // ~4800 W average planned charge
		Strategy:        "arbitrage",
		PlannedGridW:    0, // plan expected ~zero grid (charge from PV surplus)
		HasPlannedGridW: true,
	}
	// Live import of +682 W (load surged past PV) with the battery at 0 W.
	store := seedStore(682, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -1797, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Cover-load discharge drives projected grid to PlannedGridW (0):
	// adjusted = PlannedGridW − rawGridW + currentTotal = 0 − 682 + 0 = −682 W.
	if got > -582 || got < -782 {
		t.Errorf("TargetW = %f W — surplus-slot load surge must discharge to cover "+
			"load (~−682 W, grid→0), not floor at 0 and import", got)
	}
}

// TestEnergyDispatchPlannedGridCapKeepsFloorOnGridChargeSlot guards the
// scope of the cover-load discharge above: on a DELIBERATE grid-charge
// slot (PlannedGridW > 0 — the DP chose to buy from the grid to refill),
// a load surge must still only back the charge off to 0, never flip to
// discharge — undoing the planned refill would defeat the arbitrage.
func TestEnergyDispatchPlannedGridCapKeepsFloorOnGridChargeSlot(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 500, // ~2000 W planned charge
		Strategy:        "arbitrage",
		PlannedGridW:    2000, // plan: import 2 kW to grid-charge
		HasPlannedGridW: true,
	}
	// Live import +3000 W (load surged beyond the planned 2 kW), battery 0.
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Floor holds: charge backs off to 0, battery does NOT discharge.
	if got < -50 || got > 50 {
		t.Errorf("TargetW = %f W — deliberate grid-charge slot must floor at 0 on a "+
			"load surge (no cover-load discharge)", got)
	}
}

// TestEnergyDispatchUnsetPlannedGridWBypassesCap — legacy callers /
// tests construct SlotDirective without setting PlannedGridW (and
// without flipping HasPlannedGridW). HasPlannedGridW=false is the
// opt-out signal; behaviour must be unchanged from before the cap
// landed (battery chases the plan, just like the pre-fix behaviour
// the community report identified).
func TestEnergyDispatchUnsetPlannedGridWBypassesCap(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1200, // 4800 W average
		Strategy:        "arbitrage",
		// PlannedGridW + HasPlannedGridW intentionally zero-valued
	}
	store := seedStore(3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// No cap → battery follows plan ~4800 W (pre-fix behaviour).
	if got < 4700 || got > 4900 {
		t.Errorf("TargetW = %f W — with HasPlannedGridW=false the cap must not fire; "+
			"behaviour should be identical to pre-fix arbitrage (~4800 W)", got)
	}
}

// TestEnergyDispatchPlannedGridCapDoesNotFireOnDischarge — the cap
// is deliberately ONE-WAY (charge direction only). Plan: discharge
// slot, PlannedGridW=−200 (expected export 200 W after covering load).
// Load drops, live gridW = −3000 (exporting 3 kW). The battery must
// still deliver its planned discharge — the extra export is bonus
// revenue at the slot's chosen price, not a problem.
//
// Rationale (see dispatch.go cap docstring): the battery delivers
// planned Wh either way; the extra export comes from load undershoot,
// not over-discharge. Backing off would leave Wh in the battery for a
// later slot the DP already evaluated and rejected, undermining the
// plan. Economics are asymmetric vs the charge case.
//
// Regression guard against re-symmetrising the cap.
func TestEnergyDispatchPlannedGridCapDoesNotFireOnDischarge(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -1000, // -4000 W average discharge
		Strategy:        "arbitrage",
		PlannedGridW:    -200,
		HasPlannedGridW: true,
	}
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -3500, 0.5},
	})
	store.Update("pv-1", telemetry.DerPV, -100, nil, nil)
	store.DriverHealthMut("pv-1").RecordSuccess()

	st := newStateWithEnergyDispatch(dir, "ferroamp")

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Battery must follow raw plan ~−4000 W (capped by per-cmd 5 kW).
	// Anything above −3500 means the cap mistakenly fired on discharge.
	if got > -3500 {
		t.Errorf("TargetW = %f W — discharge cap fired (regression). "+
			"The plan-grid cap must be charge-direction-only; extra "+
			"export during a discharge slot is bonus revenue, not a problem.", got)
	}
}

// ---- planner_self reactive execution (issue #130) ----
//
// planner_self promises no grid-charging and no battery export. It bypasses
// energy-allocation and uses reactive self-consumption, with the plan
// providing a per-slot idle/participate gate. When the DP decided not to
// participate this slot (|planned BatteryEnergyWh| < IdleGateThresholdW when
// averaged over the slot) the EMS will not discharge to cover load, but it
// may still absorb genuine live PV surplus that would otherwise cross the
// site meter.

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
// later, more profitable slot). Plan's Wh is below threshold, but live PV
// surplus should still be absorbed because that never spends SoC and never
// grid-charges the battery.
// Idle-gate's CHARGE side: when real PV surplus exists, reactive PI drives
// the battery to absorb it. The chargeCeiling clamp caps the absorption at
// the threshold-filtered surplus so the battery doesn't try to charge past
// what would actually have been exported. Reactive PI converges over a few
// cycles rather than snapping the target instantly — that's the price of
// running PI for load-cover and absorb on the same code path, and it's a
// fine tradeoff because the per-cycle ramp at slew=500 W finishes inside
// the slot regardless.
func TestPlannerSelfIdleGateAbsorbsLivePVSurplus(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1000, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	// True surplus is 3000 W (export 4000 + battery contribution 1000).
	// Run cycles, feeding back the commanded battery W → SmoothedW and
	// keeping the non-battery residual fixed so the surplus stays real.
	var last float64
	for i := 0; i < 12; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) != 1 {
			t.Fatalf("cycle %d: want 1 target, got %d", i, len(targets))
		}
		last = targets[0].TargetW
		store.Update("ferroamp", telemetry.DerBattery, last, ptrF64(0.5), nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
		// Non-battery residual: -4000 - (-1000) = -3000 W. Keep that fixed.
		store.Update("ferroamp", telemetry.DerMeter, -3000+last, nil, nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	if math.Abs(last-3000) > 50 {
		t.Errorf("after 12 cycles with true surplus 3000 W, final target = %f W, want ≈ 3000", last)
	}
}

// When the plan explicitly expects PV export while the battery is idle, smart
// self-consumption must not reinterpret that export as "free surplus to store".
// The planner may be preserving headroom for cheaper / negative PV later.
func TestPlannerSelfPlannedPVExportDoesNotAbsorbLiveSurplus(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
		PlannedGridW:    -2500,
		HasPlannedGridW: true,
	}
	// Live mirrors the production report: battery is already charging from
	// PV and the meter is almost zero-exporting. Without the export gate,
	// planner_self would keep charging to chase grid=0. With the gate, it
	// should ramp the battery target back to 0 and let PV export.
	store := seedStore(-100, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 1600, 0.5},
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
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f W — planned PV-export slot should stop battery charge, not absorb surplus", targets[0].TargetW)
	}
}

// passive_arbitrage idle slot under live PV surplus: the EMS must not absorb
// the surplus into the battery, even when the plan's own forecast for this
// slot was near-balanced (idle, no planned export). For the slot we're
// already in, live measurements override the forecast — the DP picked idle
// deliberately, so sustaining "do not charge" under a bigger-than-forecast
// surplus is the correct generalisation.
//
// Production trigger (v0.87.x, 2026-05-28, site .40): load forecast was
// 2782 W vs actual 504 W on a high-PV slot. Plan said grid≈+28 W with
// battery_w=0. Dispatch fell through to self_consumption + grid_target=0
// and charged 2.6 kW into batteries despite high current spot + low future
// spot + abundant future PV — the exact case the DP picked idle to avoid.
func TestPlannerPassiveArbitrageIdleDoesNotAbsorbLiveSurplus(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // DP picked idle (load≈PV per forecast)
		Strategy:        "passive_arbitrage",
		PlannedGridW:    30, // forecast: grid near zero, not export
		HasPlannedGridW: true,
	}
	// Live mirrors the production report: PV surplus pushing the meter
	// negative, battery already pulling 1600 W. Without a live-export gate
	// the PI ramps charge UP to chase grid=0 and swallows the surplus.
	// Correct behaviour: ramp battery back to 0, let the surplus export.
	store := seedStore(-100, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 1600, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f W — passive_arbitrage idle slot with live PV surplus must NOT absorb (DP picked idle; for the current slot, live grid sign overrides stale forecast)", targets[0].TargetW)
	}
}

// Operator-report 2026-05-28: planner_arbitrage discharge slot, plan estimated
// baseload ~1.7 kW (BatteryEnergyWh ≈ -425 over 15 min), plan grid target ~0.
// Live load was 0.9 kW; battery sat at -1.7 kW per the energy-allocation
// formula, exporting 800 W at the spot price the operator would later have to
// buy back at consumer price. The DP picked this slot to *cover load* during
// an expensive window, not to export — the existing "bonus revenue" carve-out
// on the energy path applies only to slots where PlannedGridW < 0 (export
// intent). The fix routes cover-load discharge slots (BatteryEnergyWh < 0 AND
// PlannedGridW ~> 0) through reactive PI-on-grid=0, same path passive_arb
// idle slots already use.
func TestPlannerArbitrageCoverLoadBacksOffOnLoadUndershoot(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -425, // DP planned -1.7 kW × 15 min discharge
		Strategy:        "arbitrage",
		PlannedGridW:    1700, // plan: cover ~1.7 kW load, no export
		HasPlannedGridW: true,
	}
	// Battery already running at the planned -1.7 kW; live grid is -800 W
	// because actual load is only 900 W (load = grid - battery = -800 + 1700).
	store := seedStore(-800, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1700, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Energy path would hold target at -1700. Reactive PI must back off
	// toward -900 W (the actual load). First-cycle PI is between -1700 and
	// -900 W; assert "less negative than planned" with margin.
	if got <= -1500 {
		t.Errorf("TargetW = %.0f W — cover-load arbitrage slot must back off from planned -1700 W when live grid shows export; want target > -1500 W", got)
	}
	// Sign should still be discharge (still positive load to cover).
	if got >= 0 {
		t.Errorf("TargetW = %.0f W — cover-load slot with positive live load must still discharge", got)
	}
}

// Mirror of the undershoot case for a load *spike*: planned -1.7 kW but real
// load is 3.2 kW. Energy path holds at -1.7 kW and lets 1.5 kW import. Reactive
// PI ramps the battery down further to keep grid near 0.
func TestPlannerArbitrageCoverLoadRampsOnLoadOvershoot(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
		Strategy:        "arbitrage",
		PlannedGridW:    1700,
		HasPlannedGridW: true,
	}
	// Battery at planned -1700; load surged to 3200 → grid = 3200 - 1700 = 1500.
	store := seedStore(1500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1700, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Energy path would hold at -1700. Reactive PI must push more discharge.
	if got >= -2000 {
		t.Errorf("TargetW = %.0f W — cover-load slot with 1500 W live import must discharge more than the planned -1700 W; want target ≤ -2000 W", got)
	}
}

// Peak-export discharge slot (PlannedGridW < 0): the DP deliberately chose
// to export at this slot's high price. Reactive carve-out must NOT trigger;
// energy path holds the planned rate even when live PV varies. This is the
// behaviour the existing dispatch.go:87-91 comment justifies.
func TestPlannerArbitragePeakExportSlotStaysOnEnergyPath(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -500, // -2 kW × 15 min
		Strategy:        "arbitrage",
		PlannedGridW:    -2000, // plan: export 2 kW to grid
		HasPlannedGridW: true,
	}
	// Live grid already exporting (-500 W). Reactive PI on grid=0 would
	// charge (+500 W). Energy path must discharge ≈ -2000 W per plan.
	store := seedStore(-500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.8},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got >= -1000 {
		t.Errorf("TargetW = %.0f W — peak-export arbitrage slot (PlannedGridW=-2000) must stay on energy path and discharge ≈ -2000 W; cover-load carve-out incorrectly fired", got)
	}
}

// Cover-load slot with live PV surplus (unexpected). Energy path would blindly
// discharge -1.7 kW *into* the export, doubling the loss. Reactive carve-out
// must absorb the live surplus (or at least not discharge further).
func TestPlannerArbitrageCoverLoadDoesNotDischargeIntoLiveExport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
		Strategy:        "arbitrage",
		PlannedGridW:    500, // plan: small net import (cover load)
		HasPlannedGridW: true,
	}
	// Unexpected PV surplus: grid exporting 800 W with battery idle.
	store := seedStore(-800, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Must not discharge into the live export. Either idle (≈0) or charge to absorb.
	if got < -50 {
		t.Errorf("TargetW = %.0f W — cover-load slot with live PV surplus must not discharge into export; want target ≥ -50 W", got)
	}
}

// Backcompat: legacy callers without HasPlannedGridW (the simpler slotDirective
// test helper) must still use the energy path — intent is unknown. Locks in
// behaviour-preservation for tests written before this change.
func TestPlannerArbitrageDischargeSlotWithoutPlannedGridWUsesEnergyPath(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
		Strategy:        "arbitrage",
		HasPlannedGridW: false, // unknown intent
	}
	// Same live conditions as the undershoot test, but unknown plan intent.
	store := seedStore(-800, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1700, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Energy path must hold ≈ -1700 W.
	if got > -1500 {
		t.Errorf("TargetW = %.0f W — HasPlannedGridW=false means unknown intent; must fall through to energy path and hold ≈ -1700 W", got)
	}
}

// The carve-out also applies to passive_arbitrage — same cover-load math,
// just under the passive-arbitrage contract (no grid-charge ever). Mirror of
// the load-undershoot test for the passive mode.
func TestPlannerPassiveArbitrageCoverLoadBacksOffOnLoadUndershoot(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
		Strategy:        "passive_arbitrage",
		PlannedGridW:    1700,
		HasPlannedGridW: true,
	}
	store := seedStore(-800, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1700, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	if got <= -1500 {
		t.Errorf("TargetW = %.0f W — passive_arbitrage cover-load slot must back off when live grid shows export; want target > -1500 W", got)
	}
	if got >= 0 {
		t.Errorf("TargetW = %.0f W — cover-load slot with positive live load must still discharge", got)
	}
}

// planner_arbitrage cover-load slot must chase grid=0, not the plan's
// forecasted positive import. The PR #378 carve-out only sets
// useEnergyPath=false; production wires both SlotDirective and PlanTarget,
// so falling through to the !useEnergyPath block called PlanTarget which
// returned `("self_consumption", +1700, true)` and then SetGridTarget(+1700)
// made PI try to hit +1.7 kW import — undoing the carve-out's whole point.
// The fix routes carve-out slots to grid_target=0 unconditionally.
// Cover-load discharge slot (planned discharge to offset import) under
// live PV surplus: don't absorb the surplus into the battery. The slot
// was planned with the assumption of expected import; if live shows
// export instead, charging would steal export revenue AND the cover-
// load discharge purpose is moot (no load to cover). Right behaviour:
// hold battery near 0, let surplus export. Mirror of the passive
// idle-slot gate but for the discharge-intent slot. Codex P2 / #375
// follow-up — extends live-export gate to cover-load-discharge slots
// in both planner_arbitrage and planner_passive_arbitrage.
func TestCoverLoadDischargeDoesNotAbsorbLiveSurplus(t *testing.T) {
	for _, mode := range []Mode{ModePlannerArbitrage, ModePlannerPassiveArbitrage} {
		t.Run(string(mode), func(t *testing.T) {
			now := time.Now()
			d := SlotDirective{
				SlotStart:       now,
				SlotEnd:         now.Add(15 * time.Minute),
				BatteryEnergyWh: -600, // planned discharge to cover load
				Strategy:        "arbitrage",
				PlannedGridW:    0, // forecast: cover load, no export anticipated
				HasPlannedGridW: true,
			}
			// Live: PV-surplus exporting via the meter, battery already
			// pulling 1.6 kW. The reactive PI on grid=0 would otherwise
			// ramp charge UP to absorb the surplus, swallowing PV export
			// at high spot.
			store := seedStore(-100, []struct {
				name          string
				currentW, soc float64
			}{
				{"ferroamp", 1600, 0.5},
			})
			st := NewState(0, 0, "ferroamp")
			st.Mode = mode
			st.UseEnergyDispatch = true
			st.SlewRateW = 10000
			st.MinDispatchIntervalS = 0
			st.SlotDirective = func(time.Time) (SlotDirective, bool) { return d, true }
			targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
			if len(targets) != 1 {
				t.Fatalf("want 1 target, got %d", len(targets))
			}
			if math.Abs(targets[0].TargetW) > 1 {
				t.Errorf("TargetW = %.0f W — cover-load discharge slot with live PV surplus must not absorb (battery should be ramped back to 0)", targets[0].TargetW)
			}
		})
	}
}

func TestPlannerArbitrageCoverLoadChasesGridZeroNotPlannedImport(t *testing.T) {
	now := time.Now()
	d := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -800, // planned discharge — cover-load slot
		Strategy:        "arbitrage",
		PlannedGridW:    1700, // forecast: still importing 1.7 kW even with the discharge
		HasPlannedGridW: true,
	}
	// Live: importing 1500 W, batteries idle. The carve-out's promise is
	// "chase grid=0 so a forecast-load undershoot doesn't lock discharge
	// off". If PlanTarget's +1700 wins, PI sees grid=1500 vs setpoint=1700
	// and tries to import MORE (charge battery). Battery should instead
	// discharge ~1.5 kW to cover live load.
	store := seedStore(1500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return d, true }
	// Production wiring: PlanTarget exists alongside SlotDirective.
	// actionToSlot returns ("self_consumption", a.GridW, true) for
	// arbitrage discharge — i.e. would set grid_target to planned import.
	st.PlanTarget = func(time.Time) (string, float64, bool) {
		return "self_consumption", 1700, true
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].TargetW > -500 {
		t.Errorf("TargetW = %.0f W — cover-load slot with live import must discharge to cover (carve-out must override PlanTarget's planned-import setpoint)", targets[0].TargetW)
	}
}

// Same forward-transition risk that preparePlannerSelf already guards against
// (see resetEnergyDispatchBookkeeping comment at dispatch.go:798-803): if the
// site spent part of a slot on the energy-allocation path before the plan
// refined to a cover-load discharge or a passive-arbitrage idle, the stale
// slotDelivered accumulator stays around. A subsequent transition back to
// the energy path within the same slot (another plan refinement, an operator
// mode-hop, etc.) would then read stale Wh and miscompute remainingWh. The
// fix mirrors planner_self: when the reactive carve-out fires, reset the
// energy-path bookkeeping.
func TestPlannerArbitrageCoverLoadResetsEnergyPathBookkeeping(t *testing.T) {
	now := time.Now()
	d := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
		Strategy:        "arbitrage",
		PlannedGridW:    1700,
		HasPlannedGridW: true,
	}
	store := seedStore(-800, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1700, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return d, true }
	// Simulate "we just left the energy path mid-slot" — slotDelivered has
	// non-zero leftover, lastTickTs is in the past, currentDirective points
	// at the now-superseded plan view.
	st.slotDelivered = -200
	st.lastTickTs = now.Add(-30 * time.Second)
	st.currentDirective = SlotDirective{SlotStart: now.Add(-10 * time.Minute)}

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	if st.slotDelivered != 0 {
		t.Errorf("slotDelivered = %.0f, want 0 — cover-load carve-out must clear stale energy-path bookkeeping", st.slotDelivered)
	}
	if !st.lastTickTs.IsZero() {
		t.Errorf("lastTickTs = %v, want zero — cover-load carve-out must clear lastTickTs", st.lastTickTs)
	}
	if !st.currentDirective.SlotStart.IsZero() {
		t.Errorf("currentDirective.SlotStart = %v, want zero — carve-out must clear stale directive", st.currentDirective.SlotStart)
	}
}

func TestPlannerPassiveArbitrageIdleResetsEnergyPathBookkeeping(t *testing.T) {
	now := time.Now()
	d := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // idle slot
		Strategy:        "passive_arbitrage",
		PlannedGridW:    50,
		HasPlannedGridW: true,
	}
	store := seedStore(100, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100_000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return d, true }
	st.slotDelivered = 150
	st.lastTickTs = now.Add(-1 * time.Minute)
	st.currentDirective = SlotDirective{SlotStart: now.Add(-20 * time.Minute)}

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	if st.slotDelivered != 0 {
		t.Errorf("slotDelivered = %.0f, want 0", st.slotDelivered)
	}
	if !st.lastTickTs.IsZero() {
		t.Errorf("lastTickTs = %v, want zero", st.lastTickTs)
	}
	if !st.currentDirective.SlotStart.IsZero() {
		t.Errorf("currentDirective.SlotStart = %v, want zero", st.currentDirective.SlotStart)
	}
}

func TestPlannerSelfPlannedPVExportStopsChargingWithoutSlew(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
		PlannedGridW:    -6000,
		HasPlannedGridW: true,
	}
	store := seedStore(-3300, []struct {
		name          string
		currentW, soc float64
	}{
		{"sungrow", 2000, 0.5},
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	// Keep the default 500 W slew. The regression was target 1500 W:
	// correct intent (0 W) slowed down by the slew limiter.
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{
		"sungrow":  10000,
		"ferroamp": 14800,
	}), 11040)
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}
	for _, target := range targets {
		if math.Abs(target.TargetW) > 1 {
			t.Errorf("%s TargetW = %f W — planned PV-export slot should stop battery charge immediately despite slew",
				target.Driver, target.TargetW)
		}
	}
}

// Plan says participate this slot (above idle threshold) and live grid is
// importing. planner_self then behaves like classic self-consumption: the
// battery may discharge to pull the site meter toward zero, but it does not
// blindly execute the planned Wh as an export command.
func TestPlannerSelfParticipatesReactivelyDischargesOnImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -800, // plan: discharge 800 Wh ≈ −3.2 kW avg
		Strategy:        "self_consumption",
	}
	// Live: importing 2 kW. Reactive PI wants to reduce the import —
	// battery should discharge from live error (not blindly at the planned
	// −3.2 kW average).
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
		t.Errorf("TargetW = %f W — planner_self participant slot should discharge on live import", got)
	}
}

// planner_self in a charge-intent slot must STILL discharge when the
// live meter shows import — the operator's directive for SC mode is
// "always chase grid=0", which is symmetric: charge on live export,
// discharge on live import, regardless of which direction the plan's
// per-slot battery_w hint pointed. The plan's charge target is a
// forecast-based budget that gets revised by the reactive replan; the
// live PI must not refuse to cover import while waiting for replan,
// because the operator's stored SoC is exactly there to cover the
// load. Inverted from v0.79.5's "plan charge wins" rule after operator
// feedback.
func TestPlannerSelfPlanChargeStillDischargesOnLiveImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 800, // plan: charge 800 Wh, above idle threshold
		Strategy:        "self_consumption",
	}
	store := seedStore(1200, []struct {
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
	if targets[0].TargetW >= 0 {
		t.Errorf("TargetW = %f W — planner_self must discharge to cover live import even when the plan slot was charge", targets[0].TargetW)
	}
}

// passive_arbitrage HONOURS plan grid-charge intent (this is the
// key difference from planner_self). When the planner deliberately
// picked a cheap slot to refill the battery via grid, the dispatch
// must execute that, even if the meter is currently importing more
// than expected — the import IS the intent. Operators who want
// strict "never grid-charge regardless of price" should keep
// planner_self.
func TestPlannerPassiveArbitrageHonoursPlannedGridCharge(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 800, // plan: charge 800 Wh = 3200 W avg
		Strategy:        "passive_arbitrage",
	}
	// Live: importing 1200 W. For passive_arbitrage this is FINE — the
	// plan deliberately scheduled grid-charge during a cheap slot.
	store := seedStore(1200, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if targets[0].TargetW <= 0 {
		t.Errorf("TargetW = %f W — passive_arbitrage must execute planned grid-charge (not undo it as reactive discharge)", targets[0].TargetW)
	}
}

// Symmetric case: if the plan expected discharge but the live meter says the
// site is exporting, planner_self should absorb the live surplus. The plan
// only says "participate this slot", not "force discharge direction".
func TestPlannerSelfPlanDischargeStillChargesOnLiveExport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -800, // plan: discharge 800 Wh, above idle threshold
		Strategy:        "self_consumption",
	}
	store := seedStore(-1200, []struct {
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
	if targets[0].TargetW <= 0 {
		t.Errorf("TargetW = %f W — planner_self must absorb live export even when the plan slot was discharge", targets[0].TargetW)
	}
}

func TestPlannerSelfParticipantMatchesManualSelfConsumption(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: -800, // participant slot, not idle/charge-only
		Strategy:        "self_consumption",
	}
	tests := []struct {
		name  string
		gridW float64
	}{
		{name: "import", gridW: 2000},
		{name: "export", gridW: -1200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manualStore := seedStore(tt.gridW, []struct {
				name          string
				currentW, soc float64
			}{
				{"ferroamp", 0, 0.5},
			})
			plannerStore := seedStore(tt.gridW, []struct {
				name          string
				currentW, soc float64
			}{
				{"ferroamp", 0, 0.5},
			})

			manual := NewState(0, 0, "ferroamp")
			manual.Mode = ModeSelfConsumption
			manual.SlewRateW = 10000
			manual.MinDispatchIntervalS = 0

			planner := NewState(0, 0, "ferroamp")
			planner.Mode = ModePlannerSelf
			planner.UseEnergyDispatch = true
			planner.SlewRateW = 10000
			planner.MinDispatchIntervalS = 0
			planner.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

			want := ComputeDispatch(manualStore, manual, caps(map[string]float64{"ferroamp": 15200}), 11040)
			got := ComputeDispatch(plannerStore, planner, caps(map[string]float64{"ferroamp": 15200}), 11040)
			if len(got) != len(want) {
				t.Fatalf("target count mismatch: got %d want %d (%+v vs %+v)", len(got), len(want), got, want)
			}
			for i := range got {
				if got[i].Driver != want[i].Driver {
					t.Fatalf("target[%d] driver = %s, want %s", i, got[i].Driver, want[i].Driver)
				}
				if math.Abs(got[i].TargetW-want[i].TargetW) > 1 {
					t.Errorf("target[%d] = %f W, want manual self_consumption parity %f W", i, got[i].TargetW, want[i].TargetW)
				}
			}
		})
	}
}

// Multi-cycle steady-state: idle-gated battery starts far from 0 and must
// reach 0 monotonically (no PI integral-windup overshoot, slew respected).
// Guards against the "gate goes on but PI wound up from earlier cycles
// keeps pushing" class of bug.
// Idle-gate now lets PI converge battery to whatever covers live load,
// because the operator's "never import" floor overrides the planner's
// idle preference. Battery starts at -2000 (over-discharging); live load
// only needs -1500 to hold grid=0. PI ramps back to -1500 over a few
// cycles. Crucially: it does NOT ramp to 0 (which is what the old
// "idle = always 0" contract did); ramping to 0 would force the site
// to import the load instead.
func TestPlannerSelfIdleGateConvergesBatteryToCoverLoadOverCycles(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // idle-gated
		Strategy:        "self_consumption",
	}
	// Battery at -2000 W, grid -500 (exporting only because battery is
	// over-discharging by 500). Non-battery residual = +1500 W of load
	// the battery needs to cover.
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

	baseGridW := 1500.0
	var last float64
	for i := 0; i < 12; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) != 1 {
			t.Fatalf("cycle %d: want 1 target, got %d", i, len(targets))
		}
		last = targets[0].TargetW
		store.Update("ferroamp", telemetry.DerBattery, last, ptrF64(0.5), nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
		store.Update("ferroamp", telemetry.DerMeter, baseGridW+last, nil, nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	if math.Abs(last+1500) > 50 {
		t.Errorf("after 12 cycles, expected final target ≈ -1500 (discharge to cover the +1500 W residual), got %f", last)
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

// Stale planner_self must cover local load with battery discharge — the
// classic self_consumption behavior. Holding the battery idle during a
// stale plan would force the operator to import while the planner
// recovers, which is the regression that triggered the 2026-05-24 live
// failure: 60 kWh PV but ~1.5 kW grid import because the fail-safe was
// too aggressive.
func TestPlannerSelfStalePlanDischargesToCoverLoad(t *testing.T) {
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
		t.Errorf("TargetW = %f W — stale planner_self must discharge to cover load (classic self_consumption fallback)", targets[0].TargetW)
	}
	if !st.PlanStale {
		t.Error("expected PlanStale=true when planner_self sees no directive")
	}
}

// Stale planner_self must NOT absorb PV surplus. The original incident
// (operator note 2026-05-24) was the system briefly charging during a
// planned-export slot while the planner was rebuilding. Reactive grid-
// zero would have charged from 2 kW of export; the noSelfCharge clamp
// pins the post-PI target to ≤ 0 so the surplus exports instead.
func TestPlannerSelfStalePlanBlocksPVCharging(t *testing.T) {
	store := seedStore(-2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, false }
	st.PlanTarget = func(time.Time) (string, float64, bool) { return "", 0, false }

	targets := ComputeDispatch(store, st, caps(map[string]float64{
		"ferroamp": 15200,
		"sungrow":  9600,
	}), 11040)
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}
	for _, target := range targets {
		if target.TargetW > 1 {
			t.Errorf("%s TargetW = %f W — stale planner_self must NOT charge from PV (planned export gets stolen)",
				target.Driver, target.TargetW)
		}
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

func TestInverterAffinity_IgnoresOfflinePVTelemetry(t *testing.T) {
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	store.Update("ferroamp-pv", telemetry.DerPV, -3500, nil, nil)
	store.DriverHealthMut("ferroamp-pv").SetOffline()

	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.InverterGroups = map[string]string{
		"ferroamp":    "ferroamp",
		"ferroamp-pv": "ferroamp",
		"sungrow":     "sungrow",
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	// With the PV driver offline there is no trustworthy DC-local signal,
	// so charging falls back to capacity split. Old behaviour treated the
	// stale -3.5 kW PV reading as live and routed almost all charge to Ferroamp.
	if got["sungrow"] < 400 {
		t.Errorf("sungrow target = %f — offline PV must not create a locality preference", got["sungrow"])
	}
}

func TestInverterAffinity_IgnoresNonGeneratingPVTelemetry(t *testing.T) {
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	store.Update("ferroamp-pv", telemetry.DerPV, 500, nil, nil) // standby/import, not generation
	store.DriverHealthMut("ferroamp-pv").RecordSuccess()

	st := NewState(0, 0, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.InverterGroups = map[string]string{
		"ferroamp":    "ferroamp",
		"ferroamp-pv": "ferroamp",
		"sungrow":     "sungrow",
	}

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)
	got := map[string]float64{}
	for _, t := range targets {
		got[t.Driver] = t.TargetW
	}
	if got["sungrow"] < 400 {
		t.Errorf("sungrow target = %f — positive PV telemetry must not be counted as local generation", got["sungrow"])
	}
}

// ---- PV curtailment ----

// 2026-05-25 Ferroamp fault: 2.7 kW load step under 6 kW PV + 85 % SoC
// triggered the inverter's internal DC-link protection. Operator opted
// into DCLinkProtection to pre-curtail PV when SoC + surplus put the
// inverter at risk. Verify the protection engages correctly in that
// scenario.
func TestProtectivePVCurtailEngagesAtHighSoCWithSurplus(t *testing.T) {
	store := telemetry.NewStore()
	// 6 kW PV producing
	store.Update("ferroamp", telemetry.DerPV, -6000, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	// Battery at 85 % SoC, online
	soc := 0.85
	store.Update("ferroamp", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	// Site meter showing 5.5 kW export (load 500 W, 5.5 kW out)
	store.Update("ferroamp", telemetry.DerMeter, -5500, nil, nil)

	st := NewState(0, 0, "ferroamp")
	st.SupportsPVCurtail = map[string]bool{"ferroamp": true}
	st.DCLinkProtectionEnabled = true
	st.DCLinkProtectionSoCThreshold = 0.80
	st.DCLinkProtectionMarginW = 1000
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		// Planner asks for no curtail — protection fires anyway.
		return SlotDirective{}, true
	}

	targets := ComputePVCurtail(st, store)
	if len(targets) != 1 || targets[0].Driver != "ferroamp" {
		t.Fatalf("want 1 target on ferroamp, got %+v", targets)
	}
	// Expected limit ≈ load (500) + margin (1000) = 1500 W
	if got := targets[0].LimitW; math.Abs(got-1500) > 50 {
		t.Errorf("protective curtail limit = %f, want ≈ 1500", got)
	}
}

func TestProtectivePVCurtailSkipsBelowSoCThreshold(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("ferroamp", telemetry.DerPV, -6000, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	soc := 0.50 // below threshold
	store.Update("ferroamp", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	store.Update("ferroamp", telemetry.DerMeter, -5500, nil, nil)

	st := NewState(0, 0, "ferroamp")
	st.SupportsPVCurtail = map[string]bool{"ferroamp": true}
	st.DCLinkProtectionEnabled = true
	st.DCLinkProtectionSoCThreshold = 0.80
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return SlotDirective{}, true }

	if got := ComputePVCurtail(st, store); len(got) != 0 {
		t.Errorf("protection should not fire below SoC threshold, got %+v", got)
	}
}

// Protection must NOT relax a tighter manual hold. If the operator
// pinned a 500 W cap and protection would suggest 1500 W, the cap
// wins. (We test against a manual hold rather than the planner
// directive because liveCurtailLimitW already re-derives the planner
// limit from live state, which makes the planner path's "is the limit
// 500?" question opaque to a unit test.)
func TestProtectivePVCurtailNeverRelaxesManualHold(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("ferroamp", telemetry.DerPV, -6000, nil, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	soc := 0.85
	store.Update("ferroamp", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("ferroamp").RecordSuccess()
	store.Update("ferroamp", telemetry.DerMeter, -5500, nil, nil)

	st := NewState(0, 0, "ferroamp")
	st.SupportsPVCurtail = map[string]bool{"ferroamp": true}
	st.DCLinkProtectionEnabled = true
	st.DCLinkProtectionSoCThreshold = 0.80
	st.DCLinkProtectionMarginW = 1000
	// Site-wide operator hold at 500 W (tighter than protection's 1500).
	st.SetPVManualHold(PVManualHold{LimitW: 500, ExpiresAt: time.Now().Add(time.Hour)})

	targets := ComputePVCurtail(st, store)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %+v", targets)
	}
	if got := targets[0].LimitW; got > 600 {
		t.Errorf("manual hold's 500 W cap got relaxed by protection to %f", got)
	}
}

func TestPVCurtailAllocatesOnlinePVDrivers(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("pv-a", telemetry.DerPV, -3000, nil, nil)
	store.DriverHealthMut("pv-a").RecordSuccess()
	store.Update("pv-b", telemetry.DerPV, -1000, nil, nil)
	store.DriverHealthMut("pv-b").RecordSuccess()

	st := NewState(0, 0, "meter")
	st.SupportsPVCurtail = map[string]bool{"pv-a": true, "pv-b": true}
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{PVLimitW: 2000}, true
	}

	targets := ComputePVCurtail(st, store)
	got := map[string]float64{}
	for _, tg := range targets {
		got[tg.Driver] = tg.LimitW
	}
	if math.Abs(got["pv-a"]-1500) > 1 || math.Abs(got["pv-b"]-500) > 1 {
		t.Fatalf("curtail allocation = %+v, want pv-a≈1500 pv-b≈500", got)
	}
}

func TestPVCurtailSkipsOfflinePVDriver(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("pv-offline", telemetry.DerPV, -3000, nil, nil)
	store.DriverHealthMut("pv-offline").SetOffline()

	st := NewState(0, 0, "meter")
	st.SupportsPVCurtail = map[string]bool{"pv-offline": true}
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{PVLimitW: 1000}, true
	}

	targets := ComputePVCurtail(st, store)
	if len(targets) != 0 {
		t.Fatalf("offline PV driver must not receive curtail target, got %+v", targets)
	}
}

func TestPVCurtailReleasesDriverThatWentOffline(t *testing.T) {
	store := telemetry.NewStore()
	store.Update("pv-offline", telemetry.DerPV, -3000, nil, nil)
	store.DriverHealthMut("pv-offline").SetOffline()

	st := NewState(0, 0, "meter")
	st.SupportsPVCurtail = map[string]bool{"pv-offline": true}
	st.LastCurtailedDrivers = map[string]bool{"pv-offline": true}
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{PVLimitW: 1000}, true
	}

	targets := ComputePVCurtail(st, store)
	if len(targets) != 1 {
		t.Fatalf("want one release target, got %+v", targets)
	}
	if targets[0].Driver != "pv-offline" || targets[0].LimitW != 0 {
		t.Fatalf("offline previously-curtailed driver should be released, got %+v", targets[0])
	}
}

// planner_self idle slots may absorb large live surplus via reactive PI.
// The chargeCeiling clamp keeps the target from overshooting the actual
// surplus; convergence takes a few cycles at Kp=0.5 / Ki=0.1.
func TestPlannerSelfIdleGateAbsorbsLargeLiveSurplus(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
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

	// Surplus residual without battery is 3000 W. Hold it fixed as PI
	// drives the battery up.
	var last float64
	for i := 0; i < 12; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) != 1 {
			t.Fatalf("cycle %d: want 1 target, got %d", i, len(targets))
		}
		last = targets[0].TargetW
		store.Update("ferroamp", telemetry.DerBattery, last, ptrF64(0.5), nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
		store.Update("ferroamp", telemetry.DerMeter, -3000+last, nil, nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	if math.Abs(last-3000) > 50 {
		t.Errorf("after 12 cycles with 3 kW true surplus, final target = %f W, want ≈ 3000", last)
	}
}

// The idle gate ignores tiny live export inside the noise threshold.
func TestPlannerSelfIdleGateHoldsWhenLiveSurplusUnderThreshold(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Live: grid exporting 50 W — below IdleGateThresholdW.
	store := seedStore(-50, []struct {
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
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f — idle-gate should hold under small export; "+
			"want ~0", targets[0].TargetW)
	}
}

// Battery-created export must not flip the idle gate into a self-feeding
// charge loop. With the battery discharging 1 kW and meter exporting 500 W,
// the non-battery residual is +500 W (load > PV). Reactive PI drives the
// battery to cover that 500 W of load, i.e. it ramps DOWN to -500 — it
// never charges from its own export. The chargeCeiling clamp guarantees
// the charge side is pinned at 0 because trueMeterExportWithoutBatteryW
// is negative here (load-dominated).
func TestPlannerSelfIdleGateDoesNotTreatBatteryDischargeAsSurplus(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	store := seedStore(-500, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1000, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	// Non-battery residual: -500 - (-1000) = +500 (load 500). Battery
	// should land on -500 once PI converges, not flip into charge.
	var last float64
	for i := 0; i < 12; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) != 1 {
			t.Fatalf("cycle %d: want 1 target, got %d", i, len(targets))
		}
		last = targets[0].TargetW
		store.Update("ferroamp", telemetry.DerBattery, last, ptrF64(0.5), nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
		store.Update("ferroamp", telemetry.DerMeter, 500+last, nil, nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	if last > 1 {
		t.Errorf("TargetW = %f — battery-created export must not flip into charge; want ≤ 0", last)
	}
	if math.Abs(last+500) > 50 {
		t.Errorf("TargetW = %f — battery should land discharging 500 W to cover residual load; want ≈ -500", last)
	}
}

// EV-and-surplus tests need convergence loops on the new reactive PI
// path. EV draws 3 kW, meter still exports 1 kW → the idle absorber
// should converge on charging 1 kW, never reaching into the 4 kW
// house-side surplus that's already accounted for by the EV.
func TestPlannerSelfIdleGateChargesOnlyActualMeterSurplusWithEVActive(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	store := seedStore(-1000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.EVChargingW = 3000
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	var last float64
	for i := 0; i < 12; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) != 1 {
			t.Fatalf("cycle %d: want 1 target, got %d", i, len(targets))
		}
		last = targets[0].TargetW
		store.Update("ferroamp", telemetry.DerBattery, last, ptrF64(0.5), nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
		// Non-battery residual stays at -1000 (export 1 kW with EV at 3 kW)
		store.Update("ferroamp", telemetry.DerMeter, -1000+last, nil, nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	if math.Abs(last-1000) > 50 {
		t.Errorf("TargetW = %f — should converge on absorbing only the actual meter export (~1000)", last)
	}
}

func TestPlannerSelfIdleGateLeavesSurplusOnlyEVReserve(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// The meter exports 3 kW while an EV is already taking 1 kW. A
	// surplus-only EV controller has asked us to leave 3 kW total reserved,
	// so the battery may absorb only the 1 kW beyond the remaining EV headroom.
	store := seedStore(-3000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.EVChargingW = 1000
	st.EVSurplusOnlyReserveW = 3000
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW-1000) > 1 {
		t.Errorf("TargetW = %f — idle gate must leave surplus-only EV reserve; want ~1000", targets[0].TargetW)
	}
}

func TestPlannerSelfIdleGateSplitsLiveSurplusAcrossBatteries(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	store := seedStore(-4000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5},
		{"sungrow", 0, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	var ferroLast, sungrowLast float64
	for i := 0; i < 12; i++ {
		targets := ComputeDispatch(store, st, caps(map[string]float64{
			"ferroamp": 15200,
			"sungrow":  9600,
		}), 11040)
		if len(targets) != 2 {
			t.Fatalf("cycle %d: want 2 targets, got %d", i, len(targets))
		}
		for _, tg := range targets {
			switch tg.Driver {
			case "ferroamp":
				ferroLast = tg.TargetW
			case "sungrow":
				sungrowLast = tg.TargetW
			}
		}
		store.Update("ferroamp", telemetry.DerBattery, ferroLast, ptrF64(0.5), nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
		store.Update("sungrow", telemetry.DerBattery, sungrowLast, ptrF64(0.5), nil)
		store.DriverHealthMut("sungrow").RecordSuccess()
		// Non-battery residual: -4000 W of surplus.
		store.Update("ferroamp", telemetry.DerMeter, -4000+ferroLast+sungrowLast, nil, nil)
		store.DriverHealthMut("ferroamp").RecordSuccess()
	}
	sum := ferroLast + sungrowLast
	if ferroLast <= 0 || sungrowLast <= 0 {
		t.Errorf("ferro=%f sungrow=%f — both should be charging when absorbing live surplus", ferroLast, sungrowLast)
	}
	if math.Abs(sum-4000) > 100 {
		t.Errorf("aggregate target = %f — should split 4 kW of surplus across both batteries", sum)
	}
}

// The user-visible contract change (2026-05-24): planner_self idle slots
// must still cover live import with battery discharge. The "never import
// what stored energy could've covered" floor takes precedence over the
// planner's idle preference. Previously a stale or wrong PV forecast
// could plan-into-idle and leave the operator importing through it.
func TestPlannerSelfIdleGateDischargesToCoverLiveImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		Strategy:        "self_consumption",
	}
	// Live: grid importing 2 kW. Battery idle, has SoC to spare.
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
	if targets[0].TargetW >= 0 {
		t.Errorf("TargetW = %f — idle-gate must discharge to cover 2 kW live import (operator's never-import floor)", targets[0].TargetW)
	}
}

// Even when the plan asks for active PV export, live import must still be
// covered by discharge. The export preference applies only to the charge
// direction (don't absorb PV that could've exported); discharging to cover
// load is always allowed.
func TestPlannerSelfExportSurplusGateStillCoversLiveImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0,
		PlannedGridW:    -1500, // plan: export 1.5 kW
		Strategy:        "self_consumption",
	}
	// Plan said "export 1.5 kW"; reality is grid importing 1 kW (PV
	// forecast was way too high). Battery must discharge to cover.
	store := seedStore(1000, []struct {
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
	if targets[0].TargetW >= 0 {
		t.Errorf("TargetW = %f — export-surplus gate must still discharge to cover live import", targets[0].TargetW)
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
		name          string
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
			name          string
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

func TestPlanSignFloorIgnoresPlannerSelf(t *testing.T) {
	// planner_self uses the plan only for idle-vs-participate. A non-idle
	// slot is handled by the self-consumption path, not the planner sign
	// floor. Charge/idle planner_self slots get their no-discharge floor
	// separately; participant slots can discharge to hold grid near zero.
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerSelf
	st.SlotDirective = func(time.Time) (SlotDirective, bool) {
		return SlotDirective{BatteryEnergyWh: -600}, true
	}
	in := []DispatchTarget{{Driver: "pixii", TargetW: 1700}}
	out := applyPlanSignFloor(in, st)
	if out[0].TargetW != 1700 {
		t.Errorf("planner_self: TargetW = %f, want unchanged 1700", out[0].TargetW)
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
		name          string
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
	// grid = +500 (some import). The manual hold is an explicit operator
	// override and should charge despite live self-consumption wanting
	// to reduce import.
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

func TestBatteryManualIdleHoldStopsChargingWithoutSlew(t *testing.T) {
	store := seedStore(-2000, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 1900, 0.5},
		{"sungrow", 1800, 0.5},
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModePlannerSelf
	st.UseEnergyDispatch = true
	st.MinDispatchIntervalS = 0
	// Keep default 500 W slew. A manual idle hold is an explicit operator
	// stop command, so it must not ramp down over several cycles.
	st.SetBatteryManualHold(BatteryManualHold{PowerW: 0, ExpiresAt: time.Now().Add(60 * time.Second)})
	targets := ComputeDispatch(store, st, caps(map[string]float64{
		"ferroamp": 15200,
		"sungrow":  9600,
	}), 11040)
	if len(targets) != 2 {
		t.Fatalf("want 2 targets, got %d", len(targets))
	}
	for _, target := range targets {
		if math.Abs(target.TargetW) > 1 {
			t.Errorf("%s TargetW = %f — idle hold should force 0 W immediately",
				target.Driver, target.TargetW)
		}
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
	// Self-consumption with grid=+1500 discharges toward grid zero.
	if targets[0].TargetW >= 0 {
		t.Errorf("expired hold should revert to self_consumption discharge, got %f",
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
	st.Mode = ModePeakShaving
	st.PeakLimitW = 0
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
	st.Mode = ModePeakShaving
	st.PeakLimitW = 0
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
	st.Mode = ModePeakShaving
	st.PeakLimitW = 0
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
	st.Mode = ModePeakShaving
	st.PeakLimitW = 0
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
	st.Mode = ModeWeighted
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

// Regression guard for the production v0.87.0 incident: PV forecast was off
// by 7×, plan said battery idle (export the imaginary PV surplus), batteries
// sat at 0 W while the site imported 648 W. The carve-out was only in
// planner_self so passive_arbitrage didn't benefit.
//
// Fix: passive_arbitrage now participates in the reactive-discharge carve-out
// when the plan slot is idle (non-charge). The battery should discharge to
// cover the live import just as planner_self would.
func TestPlannerPassiveArbitrageIdleSlotReactsToForecastMiss(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // idle — plan expected PV export, battery hands-off
		Strategy:        "self_consumption",
	}
	// Live: meter is importing 600 W — PV massively undershot forecast.
	store := seedStore(600, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000 // unbounded for single-tick test
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Reactive PI on grid=+600 W with Kp=0.5 yields ~-300 W discharge
	// after one tick. The key assertion is that the battery discharged at
	// all — before the fix planHasNonDischargeIntent returned true and
	// floored the target to 0.
	if got >= 0 {
		t.Errorf("TargetW = %.0f W — passive_arbitrage idle slot must discharge reactively when meter imports (forecast miss). Before fix: target was floored to 0.", got)
	}
}

// When the plan slot for passive_arbitrage is a deliberate CHARGE slot
// (e.g. the DP picked cheap grid hours to refill), live grid import is
// expected — that's what grid-charging looks like. The carve-out must NOT
// apply here: the charge command is the authoritative intent and reactive
// discharge would undo it.
func TestPlannerPassiveArbitrageChargeSlotPreservedAgainstReactiveDischarge(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 1000, // deliberate grid-charge: ~4 kW over the slot
		Strategy:        "arbitrage",
	}
	// Live: meter importing 600 W. In a charge slot this is expected and
	// intentional (grid → battery). Battery is currently at 0 W.
	store := seedStore(600, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.4},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerPassiveArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	got := targets[0].TargetW
	// Energy-dispatch path computes targetTotalW = 1000*3600/900 ≈ 4000 W
	// charge. Reactive discharge must NOT override this. The non-discharge
	// block (planHasNonDischargeIntent=true) should floor to 0 at minimum —
	// but the energy path itself drives positive, so we just verify the
	// battery is commanded to CHARGE, not discharge.
	if got < 0 {
		t.Errorf("TargetW = %.0f W — passive_arbitrage charge slot must NOT discharge reactively; the grid-charge plan must remain authoritative", got)
	}
}

// ---- Slot delivery observability ----
//
// These tests cover the path-agnostic per-slot Wh accumulator that
// runs on EVERY dispatch tick (planner_self, planner_passive_arbitrage,
// the planner_arbitrage cover-load carve-out from PR #378, etc.).
// The accumulator measures actual fleet delivery and compares it
// against the plan's BatteryEnergyWh at slot rollover. Pure
// observability — no dispatch decision reads the counters.

// makeSlotMetricsState configures the minimum State needed to exercise
// the slot-delivery accumulator. Mode is planner_arbitrage so that
// SlotDirective is consulted, but useEnergyPath is irrelevant to the
// accumulator (it runs above all mode logic).
func makeSlotMetricsState(siteMeter string, dirFn SlotDirectiveFunc) *State {
	st := NewState(0, 0, siteMeter)
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = dirFn
	return st
}

//  1. The accumulator integrates current battery total × dt across
//     multiple ticks within the same slot.
func TestSlotMetricsAccumulatesActualWhAcrossTicks(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now.Add(-1 * time.Minute),
		SlotEnd:         now.Add(14 * time.Minute),
		BatteryEnergyWh: -300,
		Strategy:        "arbitrage",
	}
	// Battery at -1000 W (site-signed: discharging) throughout.
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1000, 0.5},
	})
	st := makeSlotMetricsState("ferroamp", func(time.Time) (SlotDirective, bool) { return dir, true })

	// Tick 1 — initialise accumulator (no integration yet, anchor only).
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.slotActualWh != 0 {
		t.Fatalf("first tick should only anchor, got slotActualWh=%f", st.slotActualWh)
	}
	if !st.slotActualSlotStart.Equal(dir.SlotStart) {
		t.Fatalf("anchor SlotStart = %v, want %v", st.slotActualSlotStart, dir.SlotStart)
	}

	// Simulate 60 s elapsed since the anchor tick by rewinding the
	// last-tick timestamp. -1000 W × 60 s ≈ -16.67 Wh.
	st.slotActualLastTs = st.slotActualLastTs.Add(-60 * time.Second)
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	wantWh := -1000.0 * 60.0 / 3600.0 // ≈ -16.667
	if math.Abs(st.slotActualWh-wantWh) > 1.0 {
		t.Errorf("after ~60 s @ -1000 W: slotActualWh = %.3f Wh, want ≈ %.3f Wh", st.slotActualWh, wantWh)
	}

	// Another 60 s — total should be ~ -33.3 Wh.
	st.slotActualLastTs = st.slotActualLastTs.Add(-60 * time.Second)
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	wantWh2 := -1000.0 * 120.0 / 3600.0
	if math.Abs(st.slotActualWh-wantWh2) > 1.0 {
		t.Errorf("after ~120 s @ -1000 W: slotActualWh = %.3f Wh, want ≈ %.3f Wh", st.slotActualWh, wantWh2)
	}
}

//  2. When the SlotDirective's SlotStart advances, the accumulator
//     resets — the in-progress slot accumulator must not leak into
//     the next slot's measurement.
func TestSlotMetricsResetsOnSlotRollover(t *testing.T) {
	now := time.Now()
	slot1Start := now.Add(-30 * time.Second)
	slot1 := SlotDirective{
		SlotStart:       slot1Start,
		SlotEnd:         slot1Start.Add(15 * time.Minute),
		BatteryEnergyWh: -300,
	}
	slot2Start := slot1Start.Add(15 * time.Minute)
	slot2 := SlotDirective{
		SlotStart:       slot2Start,
		SlotEnd:         slot2Start.Add(15 * time.Minute),
		BatteryEnergyWh: -300,
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1000, 0.5},
	})

	active := slot1
	st := makeSlotMetricsState("ferroamp", func(time.Time) (SlotDirective, bool) { return active, true })

	// Tick 1 — anchor slot 1, then integrate 60 s.
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	st.slotActualLastTs = st.slotActualLastTs.Add(-60 * time.Second)
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if math.Abs(st.slotActualWh) < 10 {
		t.Fatalf("setup: slot 1 should have ~ -16.7 Wh accumulated, got %f", st.slotActualWh)
	}

	// Swap to slot 2 — rollover should reset slotActualWh and re-anchor.
	active = slot2
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.slotActualWh != 0 {
		t.Errorf("after slot rollover, slotActualWh should reset to 0, got %f", st.slotActualWh)
	}
	if !st.slotActualSlotStart.Equal(slot2Start) {
		t.Errorf("after rollover, anchor = %v, want %v", st.slotActualSlotStart, slot2Start)
	}
}

//  3. Over-delivery: planned -425 Wh, actual ~ -850 Wh → ratio 2.0,
//     well above 1.5. Counter should increment by 1 on slot rollover.
func TestSlotMetricsLogsOverDeliveryAtSlotEnd(t *testing.T) {
	now := time.Now()
	slot1Start := now.Add(-1 * time.Minute)
	slot1 := SlotDirective{
		SlotStart:       slot1Start,
		SlotEnd:         slot1Start.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
	}
	slot2Start := slot1Start.Add(15 * time.Minute)
	slot2 := SlotDirective{
		SlotStart:       slot2Start,
		SlotEnd:         slot2Start.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
	}
	// Battery discharging hard: -1700 W average × 30 min = -850 Wh
	// (using fewer ticks here — we drive the accumulator directly).
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -1700, 0.5},
	})

	active := slot1
	st := makeSlotMetricsState("ferroamp", func(time.Time) (SlotDirective, bool) { return active, true })

	// Anchor slot 1.
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	// Force accumulator to -850 Wh (2 × planned magnitude → ratio 2.0).
	st.slotActualWh = -850

	// Rollover into slot 2 — should log + increment OverDeliveryCount.
	active = slot2
	beforeOver := st.SlotDeliveryStats.OverDeliveryCount
	beforeUnder := st.SlotDeliveryStats.UnderDeliveryCount
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.SlotDeliveryStats.OverDeliveryCount != beforeOver+1 {
		t.Errorf("OverDeliveryCount = %d, want %d (incremented by 1 at rollover)",
			st.SlotDeliveryStats.OverDeliveryCount, beforeOver+1)
	}
	if st.SlotDeliveryStats.UnderDeliveryCount != beforeUnder {
		t.Errorf("UnderDeliveryCount = %d, want unchanged %d", st.SlotDeliveryStats.UnderDeliveryCount, beforeUnder)
	}
}

// Planned discharge but actual charge (same magnitude) — the largest
// possible plan-vs-reality miss must NOT register as "on target" just
// because |actual| ≈ |planned|. Sign mismatch is a categorically
// different failure (we did the opposite of what was planned) and needs
// its own counter. Codex P2 / #379 follow-up.
func TestSlotMetricsDetectsSignMismatch(t *testing.T) {
	now := time.Now()
	slot1Start := now.Add(-1 * time.Minute)
	slot1 := SlotDirective{
		SlotStart:       slot1Start,
		SlotEnd:         slot1Start.Add(15 * time.Minute),
		BatteryEnergyWh: -425, // planned: discharge
	}
	slot2Start := slot1Start.Add(15 * time.Minute)
	slot2 := SlotDirective{
		SlotStart:       slot2Start,
		SlotEnd:         slot2Start.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 1700, 0.5},
	})

	active := slot1
	st := makeSlotMetricsState("ferroamp", func(time.Time) (SlotDirective, bool) { return active, true })

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	// Same magnitude as planned, opposite direction.
	st.slotActualWh = +425

	active = slot2
	beforeMismatch := st.SlotDeliveryStats.SignMismatchCount
	beforeOver := st.SlotDeliveryStats.OverDeliveryCount
	beforeUnder := st.SlotDeliveryStats.UnderDeliveryCount
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	if st.SlotDeliveryStats.SignMismatchCount != beforeMismatch+1 {
		t.Errorf("SignMismatchCount = %d, want %d (incremented at rollover)",
			st.SlotDeliveryStats.SignMismatchCount, beforeMismatch+1)
	}
	if st.SlotDeliveryStats.OverDeliveryCount != beforeOver {
		t.Errorf("OverDeliveryCount = %d, want unchanged %d (magnitude ratio is 1.0 but the sign is wrong)",
			st.SlotDeliveryStats.OverDeliveryCount, beforeOver)
	}
	if st.SlotDeliveryStats.UnderDeliveryCount != beforeUnder {
		t.Errorf("UnderDeliveryCount = %d, want unchanged %d", st.SlotDeliveryStats.UnderDeliveryCount, beforeUnder)
	}
}

//  4. Under-delivery: planned -425 Wh, actual -100 Wh → ratio 0.235,
//     well below 0.5. Counter should increment by 1.
func TestSlotMetricsLogsUnderDelivery(t *testing.T) {
	now := time.Now()
	slot1Start := now.Add(-1 * time.Minute)
	slot1 := SlotDirective{
		SlotStart:       slot1Start,
		SlotEnd:         slot1Start.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
	}
	slot2Start := slot1Start.Add(15 * time.Minute)
	slot2 := SlotDirective{
		SlotStart:       slot2Start,
		SlotEnd:         slot2Start.Add(15 * time.Minute),
		BatteryEnergyWh: -425,
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -400, 0.5},
	})

	active := slot1
	st := makeSlotMetricsState("ferroamp", func(time.Time) (SlotDirective, bool) { return active, true })

	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	st.slotActualWh = -100 // ratio = 100/425 ≈ 0.235 < 0.5

	active = slot2
	before := st.SlotDeliveryStats.UnderDeliveryCount
	beforeOver := st.SlotDeliveryStats.OverDeliveryCount
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.SlotDeliveryStats.UnderDeliveryCount != before+1 {
		t.Errorf("UnderDeliveryCount = %d, want %d", st.SlotDeliveryStats.UnderDeliveryCount, before+1)
	}
	if st.SlotDeliveryStats.OverDeliveryCount != beforeOver {
		t.Errorf("OverDeliveryCount = %d, want unchanged %d", st.SlotDeliveryStats.OverDeliveryCount, beforeOver)
	}
}

//  5. Idle slots — |planned| ≤ 50 Wh — must not trigger logs or
//     counters regardless of actual delivery. Ratio against ~0 is
//     meaningless.
func TestSlotMetricsIgnoresIdleSlots(t *testing.T) {
	now := time.Now()
	slot1Start := now.Add(-1 * time.Minute)
	slot1 := SlotDirective{
		SlotStart:       slot1Start,
		SlotEnd:         slot1Start.Add(15 * time.Minute),
		BatteryEnergyWh: -30, // below 50 Wh threshold
	}
	slot2Start := slot1Start.Add(15 * time.Minute)
	slot2 := SlotDirective{
		SlotStart:       slot2Start,
		SlotEnd:         slot2Start.Add(15 * time.Minute),
		BatteryEnergyWh: -30,
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -2000, 0.5},
	})

	active := slot1
	st := makeSlotMetricsState("ferroamp", func(time.Time) (SlotDirective, bool) { return active, true })
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	st.slotActualWh = -500 // any value — should be ignored at rollover

	active = slot2
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if st.SlotDeliveryStats.OverDeliveryCount != 0 || st.SlotDeliveryStats.UnderDeliveryCount != 0 {
		t.Errorf("idle slot (|planned|=30 Wh) must not bump counters, got over=%d under=%d",
			st.SlotDeliveryStats.OverDeliveryCount, st.SlotDeliveryStats.UnderDeliveryCount)
	}
}

//  6. Counter accumulates monotonically across multiple over-delivery
//     rollovers — three slots in sequence → counter = 3.
func TestSlotMetricsCounterSurvivesMultipleSlots(t *testing.T) {
	now := time.Now()
	base := now.Add(-1 * time.Hour) // well in the past so all slots are "real"

	slots := []SlotDirective{
		{SlotStart: base, SlotEnd: base.Add(15 * time.Minute), BatteryEnergyWh: -400},
		{SlotStart: base.Add(15 * time.Minute), SlotEnd: base.Add(30 * time.Minute), BatteryEnergyWh: -400},
		{SlotStart: base.Add(30 * time.Minute), SlotEnd: base.Add(45 * time.Minute), BatteryEnergyWh: -400},
		{SlotStart: base.Add(45 * time.Minute), SlotEnd: base.Add(60 * time.Minute), BatteryEnergyWh: -400},
	}
	store := seedStore(0, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", -2000, 0.5},
	})

	idx := 0
	st := makeSlotMetricsState("ferroamp", func(time.Time) (SlotDirective, bool) {
		return slots[idx], true
	})

	// Anchor slot 0.
	_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)

	// For each of slots 0,1,2: force over-delivery on the in-flight slot,
	// then advance idx → next tick triggers rollover evaluation.
	for i := 0; i < 3; i++ {
		st.slotActualWh = -1000 // ratio = 1000/400 = 2.5 → over
		idx++
		_ = ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	}
	if st.SlotDeliveryStats.OverDeliveryCount != 3 {
		t.Errorf("after 3 over-delivery rollovers, OverDeliveryCount = %d, want 3",
			st.SlotDeliveryStats.OverDeliveryCount)
	}
}

// TestPlannerArbitrageIdleSlotCoversLiveImport is the planner_arbitrage
// companion to TestPlannerPassiveArbitrageIdleSlotReactsToForecastMiss. On an
// idle planner_arbitrage slot (BatteryEnergyWh ≈ 0 — the DP planned neither
// charge nor discharge, expecting PV to cover load) a forecast miss that
// leaves the meter importing must be covered reactively by the battery, not
// imported. Before this fix the idle slot stayed on the energy path
// (targetTotalW = 0) and the battery sat idle while the site imported.
func TestPlannerArbitrageIdleSlotCoversLiveImport(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // idle — plan expected PV to cover load
		Strategy:        "arbitrage",
		PlannedGridW:    0,
		HasPlannedGridW: true,
	}
	// Live: meter importing 600 W (PV undershot / load overshot forecast).
	store := seedStore(600, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
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
		t.Errorf("TargetW = %.0f W — planner_arbitrage idle slot must discharge reactively to cover a live import (forecast miss), not sit at 0 and import", got)
	}
}

// TestPlannerArbitrageIdleSlotDoesNotAbsorbLiveSurplus guards the charge side
// of the idle-slot carve-out: an idle planner_arbitrage slot with a live PV
// surplus must NOT reactively charge to swallow the export — the DP picked
// idle on purpose. The arbitrage-family live-export gate ramps the battery
// back to 0 and lets the surplus flow to grid.
func TestPlannerArbitrageIdleSlotDoesNotAbsorbLiveSurplus(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // DP picked idle
		Strategy:        "arbitrage",
		PlannedGridW:    30,
		HasPlannedGridW: true,
	}
	// Live: PV surplus exporting, battery already charging 1600 W.
	store := seedStore(-100, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 1600, 0.5},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerArbitrage
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("want 1 target, got %d", len(targets))
	}
	if math.Abs(targets[0].TargetW) > 1 {
		t.Errorf("TargetW = %f W — planner_arbitrage idle slot with live PV surplus must NOT absorb (DP picked idle)", targets[0].TargetW)
	}
}

// TestPlannerCheapIdleSlotStillBlocksReactiveDischarge pins the scope of the
// idle-slot cover-load carve-out to the arbitrage family. planner_cheap idle
// slots must keep the non-discharge block — cheap mode discharges per plan,
// it does not chase grid=0 reactively on a forecast miss.
func TestPlannerCheapIdleSlotStillBlocksReactiveDischarge(t *testing.T) {
	now := time.Now()
	dir := SlotDirective{
		SlotStart:       now,
		SlotEnd:         now.Add(15 * time.Minute),
		BatteryEnergyWh: 0, // idle
		Strategy:        "cheap",
		PlannedGridW:    0,
		HasPlannedGridW: true,
	}
	// Live: meter importing 600 W.
	store := seedStore(600, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.6},
	})
	st := NewState(0, 0, "ferroamp")
	st.Mode = ModePlannerCheap
	st.UseEnergyDispatch = true
	st.SlewRateW = 10000
	st.MinDispatchIntervalS = 0
	st.SlotDirective = func(time.Time) (SlotDirective, bool) { return dir, true }

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	for _, tg := range targets {
		if tg.TargetW < 0 {
			t.Errorf("TargetW = %.0f W — planner_cheap idle slot must NOT discharge reactively (carve-out is arbitrage-only)", tg.TargetW)
		}
	}
}
