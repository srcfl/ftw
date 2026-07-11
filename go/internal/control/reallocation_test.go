package control

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// targetsByDriver indexes a dispatch result by driver name for assertions.
func targetsByDriver(targets []DispatchTarget) map[string]DispatchTarget {
	m := make(map[string]DispatchTarget, len(targets))
	for _, t := range targets {
		m[t.Driver] = t
	}
	return m
}

// A battery that reports it cannot discharge right now (e.g. a Ferroamp
// ESO floored at its SoC limit) must be excluded from the discharge split,
// and its share reallocated to a capable sibling — not leaked to the grid.
func TestDischargeReallocatesAroundIncapableBattery(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, 2000, nil, nil) // importing 2000 W
	s.DriverHealthMut("meter").RecordSuccess()

	socF := 0.5
	// ferroamp: healthy SoC but signals it can't discharge this cycle.
	s.Update("ferroamp", telemetry.DerBattery, 0, &socF,
		json.RawMessage(`{"discharge_capable":false,"charge_capable":true}`))
	s.DriverHealthMut("ferroamp").RecordSuccess()

	socS := 0.5
	// sungrow: no capability fields → assumed capable (back-compat).
	s.Update("sungrow", telemetry.DerBattery, 0, &socS, nil)
	s.DriverHealthMut("sungrow").RecordSuccess()

	st := NewState(0, 50, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000 // disable slew so a single cycle reaches the target
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(s, st,
		caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)

	got := targetsByDriver(targets)
	// Incapable battery must be parked at ~0 for the discharge.
	if math.Abs(got["ferroamp"].TargetW) > 1 {
		t.Errorf("incapable ferroamp should get ~0 W, got %.1f", got["ferroamp"].TargetW)
	}
	// Capable sibling must absorb the discharge (negative = discharge).
	if got["sungrow"].TargetW >= -1 {
		t.Errorf("capable sungrow should absorb the discharge, got %.1f", got["sungrow"].TargetW)
	}
}

// Symmetric to the discharge case: a battery at its charge ceiling
// (charge_capable=false) is excluded from the charge split; the capable
// sibling absorbs the charge.
func TestChargeReallocatesAroundFullBattery(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, -2000, nil, nil) // exporting 2000 W
	s.DriverHealthMut("meter").RecordSuccess()

	socF := 0.5
	s.Update("ferroamp", telemetry.DerBattery, 0, &socF,
		json.RawMessage(`{"discharge_capable":true,"charge_capable":false}`))
	s.DriverHealthMut("ferroamp").RecordSuccess()

	socS := 0.5
	s.Update("sungrow", telemetry.DerBattery, 0, &socS, nil)
	s.DriverHealthMut("sungrow").RecordSuccess()

	st := NewState(0, 50, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(s, st,
		caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)

	got := targetsByDriver(targets)
	if math.Abs(got["ferroamp"].TargetW) > 1 {
		t.Errorf("full ferroamp should get ~0 W, got %.1f", got["ferroamp"].TargetW)
	}
	if got["sungrow"].TargetW <= 1 {
		t.Errorf("capable sungrow should absorb the charge (positive), got %.1f", got["sungrow"].TargetW)
	}
}

// When the capable sibling can't absorb the whole demand (its per-driver
// maxDischargeW caps it), it delivers what it can and the remainder is left
// to the grid — the reallocation never forces a battery past its cap.
func TestReallocationRespectsCapableSiblingCap(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, 3000, nil, nil) // importing 3000 W
	s.DriverHealthMut("meter").RecordSuccess()

	socF := 0.5
	s.Update("ferroamp", telemetry.DerBattery, 0, &socF,
		json.RawMessage(`{"discharge_capable":false}`))
	s.DriverHealthMut("ferroamp").RecordSuccess()

	socS := 0.5
	s.Update("sungrow", telemetry.DerBattery, 0, &socS, nil)
	s.DriverHealthMut("sungrow").RecordSuccess()

	st := NewState(0, 50, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0
	// Cap the only capable battery well below the demand.
	st.DriverLimits = map[string]PowerLimits{"sungrow": {MaxDischargeW: 300}}

	targets := ComputeDispatch(s, st,
		caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)

	got := targetsByDriver(targets)
	if math.Abs(got["ferroamp"].TargetW) > 1 {
		t.Errorf("incapable ferroamp should get ~0 W, got %.1f", got["ferroamp"].TargetW)
	}
	if math.Abs(got["sungrow"].TargetW+300) > 1 {
		t.Errorf("capable sungrow should cap at its -300 W limit, got %.1f", got["sungrow"].TargetW)
	}
	if !got["sungrow"].Clamped {
		t.Errorf("sungrow should be marked clamped at its cap")
	}
}

// When no battery can move in the demanded direction, there is nothing to
// reallocate to: the split must not crash or divide by zero, and both
// batteries stay parked (residual goes to grid).
func TestReallocationAllIncapableNoCrash(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, 2000, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()

	socF := 0.5
	s.Update("ferroamp", telemetry.DerBattery, 0, &socF,
		json.RawMessage(`{"discharge_capable":false}`))
	s.DriverHealthMut("ferroamp").RecordSuccess()
	socS := 0.5
	s.Update("sungrow", telemetry.DerBattery, 0, &socS,
		json.RawMessage(`{"discharge_capable":false}`))
	s.DriverHealthMut("sungrow").RecordSuccess()

	st := NewState(0, 50, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(s, st,
		caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)

	got := targetsByDriver(targets)
	// Both incapable to discharge → both must stay ~0 (no phantom discharge).
	for _, name := range []string{"ferroamp", "sungrow"} {
		if got[name].TargetW < -1 {
			t.Errorf("%s is discharge-incapable but got %.1f W", name, got[name].TargetW)
		}
	}
}

// Back-compat guard: with no capability fields in the emit, the split is
// identical to the pure capacity-proportional behaviour (both batteries
// share the discharge in proportion to capacity).
func TestNoCapabilityFieldsKeepsProportionalSplit(t *testing.T) {
	s := telemetry.NewStore()
	s.Update("meter", telemetry.DerMeter, 2000, nil, nil)
	s.DriverHealthMut("meter").RecordSuccess()

	socF := 0.5
	s.Update("ferroamp", telemetry.DerBattery, 0, &socF, nil)
	s.DriverHealthMut("ferroamp").RecordSuccess()
	socS := 0.5
	s.Update("sungrow", telemetry.DerBattery, 0, &socS, nil)
	s.DriverHealthMut("sungrow").RecordSuccess()

	st := NewState(0, 50, "meter")
	st.Mode = ModeSelfConsumption
	st.SlewRateW = 100000
	st.MinDispatchIntervalS = 0

	targets := ComputeDispatch(s, st,
		caps(map[string]float64{"ferroamp": 15200, "sungrow": 9600}), 11040)

	got := targetsByDriver(targets)
	// Both discharge, and ferroamp (larger capacity) carries the larger share.
	if got["ferroamp"].TargetW >= -1 || got["sungrow"].TargetW >= -1 {
		t.Fatalf("both should discharge: ferroamp=%.1f sungrow=%.1f",
			got["ferroamp"].TargetW, got["sungrow"].TargetW)
	}
	if got["ferroamp"].TargetW >= got["sungrow"].TargetW {
		t.Errorf("larger-capacity ferroamp should carry the larger discharge share: ferroamp=%.1f sungrow=%.1f",
			got["ferroamp"].TargetW, got["sungrow"].TargetW)
	}
}
