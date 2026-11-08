package loadpoint

import (
	"math"
	"testing"
	"time"
)

// A car whose own charger takes less than the charger's lowest step still
// gets that step, rather than none; one that takes more than the charger
// leaves the charger's limits alone.
func TestLimitPowerKeepsTheChargersLowestStep(t *testing.T) {
	st := State{MinChargeW: 4140, MaxChargeW: 11000, CarMaxW: 3700}
	minW, maxW, steps := LimitPower(st, []float64{4140, 7360, 11000})
	if len(steps) != 1 || steps[0] != 4140 || maxW != 4140 || minW != 4140 {
		t.Fatalf("power %v–%v %v, want the charger's lowest step", minW, maxW, steps)
	}
	st.CarMaxW = 7400
	if minW, maxW, steps = LimitPower(st, []float64{4140, 7360, 11000}); len(steps) != 2 || maxW != 7400 || minW != 4140 {
		t.Fatalf("power %v–%v %v", minW, maxW, steps)
	}
	st.CarMaxW = 22000
	if _, maxW, steps = LimitPower(st, []float64{4140, 7360, 11000}); len(steps) != 3 || maxW != 11000 {
		t.Fatalf("a car above the charger: %v %v", maxW, steps)
	}
	st.CarMaxW = 3680
	if minW, maxW, steps = LimitPower(st, nil); len(steps) != 0 || maxW != 4140 || minW != 4140 {
		t.Fatalf("a 3.7 kW car on a stepless charger: power %v–%v %v, want the charger's minimum", minW, maxW, steps)
	}
}

// deadlineNeed is the reserve's need within the limits the plan gets on a
// 3 × 25 A site, phase mode unset.
func deadlineNeed(st State) (energyWh, powerW float64, ok bool) {
	cfg := Config{MinChargeW: st.MinChargeW, MaxChargeW: st.MaxChargeW, AllowedStepsW: st.AllowedStepsW}
	minW, maxW, steps := PlanningLimits(st, cfg, SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: 3})
	return DeadlineNeed(st, minW, maxW, steps)
}

// The reserve's need and on-power hold to the session car's own charger and
// its charge limit, as the plan does.
func TestDeadlineNeedHoldsToTheCar(t *testing.T) {
	st := State{PluggedIn: true, VehicleCapacityWh: 40000, CurrentSoC: .3, TargetSoC: .8, TargetTime: time.Now().Add(3 * time.Hour),
		MinChargeW: 4140, MaxChargeW: 11000, AllowedStepsW: []float64{0, 4140, 7360, 11000}}
	if _, w, ok := deadlineNeed(st); !ok || w != 11000 {
		t.Fatalf("no car limit: %v %v", w, ok)
	}
	st.CarMaxW = 7400
	if _, w, ok := deadlineNeed(st); !ok || w != 7360 {
		t.Fatalf("a 7.4 kW car: %v %v", w, ok)
	}
	st.AllowedStepsW = nil
	if _, w, ok := deadlineNeed(st); !ok || w != 7400 {
		t.Fatalf("a 7.4 kW car on a stepless charger: %v %v", w, ok)
	}
	st.CarMaxW = 3680
	if _, w, ok := deadlineNeed(st); !ok || w != 4140 {
		t.Fatalf("a 3.7 kW car on a stepless charger: %v %v, want the charger's minimum", w, ok)
	}
	st.CarMaxW = 7400
	st.CarChargeLimit = .6
	if need, _, ok := deadlineNeed(st); !ok || math.Abs(need-(.6-.3)*40000/DefaultChargeEfficiency) > 1e-6 {
		t.Fatalf("a 60 %% limit: %v %v", need, ok)
	}
	st.CurrentSoC = .6
	if _, _, ok := deadlineNeed(st); ok {
		t.Fatal("a car at its own limit still needs energy")
	}
}

// Review of #1807: on a table with one-phase and three-phase levels, planned
// charging runs on three phases. A car whose own maximum is below the
// lowest three-phase level got a one-phase level as its on-power, which no
// three-phase order has, and was ordered 0 W. It gets that lowest level, as
// the plan gives it. A car marked one-phase gets the third of a three-phase
// level it takes, up to its own maximum, on that table and on one with
// three-phase levels only.
func TestDeadlineNeedRunsOnThePlansPhases(t *testing.T) {
	var mixed, threePhase []float64
	for a := 6.0; a <= 16; a++ {
		mixed, threePhase = append(mixed, a*230), append(threePhase, a*690)
	}
	mixed = append(append([]float64{0}, mixed...), threePhase...)
	threePhase = append([]float64{0}, threePhase...)
	for _, c := range []struct {
		name      string
		minW      float64
		steps     []float64
		carMaxW   float64
		carPhases int
		want      float64
	}{
		{"mixed table, a 3.7 kW car", 1380, mixed, 3680, 0, 4140},
		{"mixed table, a 3.7 kW car marked one-phase", 1380, mixed, 3680, 1, 3680},
		{"mixed table, a 2.3 kW car marked one-phase", 1380, mixed, 2300, 1, 2300},
		{"three-phase table, a 3.7 kW car", 4140, threePhase, 3680, 0, 4140},
		{"three-phase table, a 2.3 kW car marked one-phase", 4140, threePhase, 2300, 1, 2300},
	} {
		st := State{PluggedIn: true, VehicleCapacityWh: 40000, CurrentSoC: .3, TargetSoC: .8, TargetTime: time.Now().Add(3 * time.Hour),
			MinChargeW: c.minW, MaxChargeW: 11040, AllowedStepsW: c.steps, CarMaxW: c.carMaxW, CarPhases: c.carPhases}
		if _, w, ok := deadlineNeed(st); !ok || w != c.want {
			t.Errorf("%s: on-power %v ok=%v, want %v", c.name, w, ok, c.want)
		}
	}
}

// The controller's limits read the loadpoint's phase mode and the site fuse
// as they stand. A three-phase site keeps a mixed table's three-phase levels;
// a 1p pin or a one-phase site keeps its one-phase levels.
func TestControllerPlanningLimitsReadPhaseModeAndSite(t *testing.T) {
	mixed := []float64{0}
	for _, phases := range []float64{1, 3} {
		for a := 6.0; a <= 16; a++ {
			mixed = append(mixed, a*230*phases)
		}
	}
	st := State{ID: "garage", MinChargeW: 1380, MaxChargeW: 11040, AllowedStepsW: mixed, CarMaxW: 3680}
	for _, c := range []struct {
		name   string
		mode   string
		phases int
		want   float64
	}{
		{"three-phase site", "", 3, 4140},
		{"three-phase site, auto", "auto", 3, 4140},
		{"pinned 1p", "1p", 3, 3680},
		{"one-phase site", "", 1, 3680},
	} {
		m := NewManager()
		m.Load([]Config{{ID: "garage", DriverName: "easee", PhaseMode: c.mode, MinChargeW: 1380, MaxChargeW: 11040, AllowedStepsW: mixed}})
		ctrl := NewController(m, nil, nil, nil)
		ctrl.SetSiteFuse(SiteFuse{MaxAmps: 25, Voltage: 230, PhaseCnt: c.phases})
		if _, maxW, _ := ctrl.PlanningLimits(st); maxW != c.want {
			t.Errorf("%s: top %v W, want %v W", c.name, maxW, c.want)
		}
	}
	if _, maxW, _ := (*Controller)(nil).PlanningLimits(st); maxW != 3680 {
		t.Errorf("no controller: top %v W, want the charger's own table", maxW)
	}
}
