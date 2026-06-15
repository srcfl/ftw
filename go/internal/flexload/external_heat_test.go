package flexload

import (
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/thermalmodel"
)

// TestStoveDetectionFiresOnUnexplainedWarming simulates a wood stove: the
// room warms fast while metered electric heat is ~0. The detector should go
// active and infer a positive external power.
func TestStoveDetectionFiresOnUnexplainedWarming(t *testing.T) {
	m := thermalmodel.NewModel()
	m.Samples = thermalmodel.WarmupSamples
	det := &ExternalHeatDetector{}

	now := int64(1_700_000_000_000)
	indoor, outdoor := 20.0, 0.0
	const dt = 300.0 // 5-min steps

	// Stove drives +1.0°C every 5 min (= 12°C/h) with zero metered power.
	for i := 0; i < 6; i++ {
		next := indoor + 1.0
		expDelta := m.ExpectedDeltaC(indoor, outdoor, 0, dt) // model expects slight cooling
		obsDelta := next - indoor
		det.Update(obsDelta, expDelta, 0 /*metered W*/, dt, now, m.ThermalWForRate)
		indoor = next
		now += int64(dt) * 1000
	}
	if !det.Active(now) {
		t.Fatal("expected stove detector to be active during unexplained warming")
	}
	if det.EstThermalW <= 0 {
		t.Errorf("expected positive inferred external power, got %.0f", det.EstThermalW)
	}
}

// TestStoveDetectionIgnoresOurOwnHeating ensures the detector does NOT fire
// when the warming is explained by our own metered electric heat.
func TestStoveDetectionIgnoresOurOwnHeating(t *testing.T) {
	m := thermalmodel.NewModel()
	m.Samples = thermalmodel.WarmupSamples
	det := &ExternalHeatDetector{}

	now := int64(1_700_000_000_000)
	indoor, outdoor := 20.0, 5.0
	const dt = 300.0
	for i := 0; i < 10; i++ {
		// Heat with 2 kW; the observed delta matches the model exactly.
		exp := m.ExpectedDeltaC(indoor, outdoor, 2000, dt)
		det.Update(exp /*observed == expected*/, exp, 2000 /*metered W, above threshold*/, dt, now, m.ThermalWForRate)
		indoor += exp
		now += int64(dt) * 1000
	}
	if det.Active(now) {
		t.Error("detector must not fire when warming is explained by metered heating")
	}
}

// TestStoveCycleFoldsEnergy verifies a firing's energy is folded into the
// per-cycle average once the hold window lapses with no further detection.
func TestStoveCycleFoldsEnergy(t *testing.T) {
	m := thermalmodel.NewModel()
	m.Samples = thermalmodel.WarmupSamples
	det := &ExternalHeatDetector{}

	now := int64(1_700_000_000_000)
	indoor, outdoor := 19.0, -5.0
	const dt = 300.0
	// Fire for ~1h (12 steps).
	for i := 0; i < 12; i++ {
		next := indoor + 0.8
		exp := m.ExpectedDeltaC(indoor, outdoor, 0, dt)
		det.Update(next-indoor, exp, 0, dt, now, m.ThermalWForRate)
		indoor = next
		now += int64(dt) * 1000
	}
	// Now idle past the hold window with no unexplained warming → cycle closes.
	now += extHeatHoldMs + 1
	det.Update(0, 0, 0, dt, now, m.ThermalWForRate)
	if det.Active(now) {
		t.Error("cycle should have closed after the hold window")
	}
	if det.Cycles != 1 {
		t.Errorf("expected 1 completed cycle, got %d", det.Cycles)
	}
	if det.AvgCycleWh <= 0 {
		t.Errorf("expected a positive learned per-cycle energy, got %.0f", det.AvgCycleWh)
	}
}

// TestArbitrationShedsLeastEfficientFirst verifies the COP-aware ordering:
// between two same-power zones, the low-COP (resistive) one is blocked
// before the high-COP (heat-pump) one.
func TestArbitrationShedsLeastEfficientFirst(t *testing.T) {
	m := trainedModel()
	specs := []SimpleSpec{
		{Model: m, CurrentC: 21.5, TargetC: 20, MinC: 18, Outdoor: 5, MaxHeatW: 2000, COP: 3, BlockHorizon: 3_600_000_000_000}, // heat pump
		{Model: m, CurrentC: 21.5, TargetC: 20, MinC: 18, Outdoor: 5, MaxHeatW: 2000, COP: 1, BlockHorizon: 3_600_000_000_000}, // resistive
	}
	decisions := []SimpleDecision{
		{Heat: true, SetpointC: 20, EstHeatW: 700, CoastHours: 5}, // 2000/3 ≈ 667 elec
		{Heat: true, SetpointC: 20, EstHeatW: 2000, CoastHours: 5},
	}
	// Budget fits only one of the two electrical draws.
	ArbitrateSimple(decisions, specs, 800)
	if !decisions[0].Heat {
		t.Error("high-COP heat pump should keep running (most heat per watt)")
	}
	if decisions[1].Heat {
		t.Error("low-COP resistive zone should be shed first")
	}
}
