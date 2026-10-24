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

// The reserve's need and on-power hold to the session car's own charger and
// its charge limit, as the plan does.
func TestDeadlineNeedHoldsToTheCar(t *testing.T) {
	st := State{PluggedIn: true, VehicleCapacityWh: 40000, CurrentSoC: .3, TargetSoC: .8, TargetTime: time.Now().Add(3 * time.Hour),
		MinChargeW: 4140, MaxChargeW: 11000, AllowedStepsW: []float64{0, 4140, 7360, 11000}}
	if _, w, ok := DeadlineNeed(st); !ok || w != 11000 {
		t.Fatalf("no car limit: %v %v", w, ok)
	}
	st.CarMaxW = 7400
	if _, w, ok := DeadlineNeed(st); !ok || w != 7360 {
		t.Fatalf("a 7.4 kW car: %v %v", w, ok)
	}
	st.AllowedStepsW = nil
	if _, w, ok := DeadlineNeed(st); !ok || w != 7400 {
		t.Fatalf("a 7.4 kW car on a stepless charger: %v %v", w, ok)
	}
	st.CarMaxW = 3680
	if _, w, ok := DeadlineNeed(st); !ok || w != 4140 {
		t.Fatalf("a 3.7 kW car on a stepless charger: %v %v, want the charger's minimum", w, ok)
	}
	st.CarMaxW = 7400
	st.CarChargeLimit = .6
	if need, _, ok := DeadlineNeed(st); !ok || math.Abs(need-(.6-.3)*40000/DefaultChargeEfficiency) > 1e-6 {
		t.Fatalf("a 60 %% limit: %v %v", need, ok)
	}
	st.CurrentSoC = .6
	if _, _, ok := DeadlineNeed(st); ok {
		t.Fatal("a car at its own limit still needs energy")
	}
}
