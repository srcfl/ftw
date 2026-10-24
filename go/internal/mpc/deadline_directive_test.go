package mpc

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func deadlineGoalState(now time.Time, id string) loadpoint.State {
	return loadpoint.State{ID: id, DriverName: "charger-" + id, PluggedIn: true, VehicleCapacityWh: 60000, CurrentSoC: .3, TargetSoC: .8, TargetTime: now.Add(3 * time.Hour), MinChargeW: 1380, MaxChargeW: 11000, AllowedStepsW: []float64{0, 1380, 4140, 6900, 11000}}
}

func TestGoalReserveWhenNoAcceptedPlan(t *testing.T) {
	now := time.Now()
	for _, n := range []int{1, 2} {
		t.Run(string(rune('0'+n))+"cars", func(t *testing.T) {
			states := []loadpoint.State{deadlineGoalState(now, "a")}
			if n == 2 {
				states = append(states, deadlineGoalState(now, "b"))
			}
			for i := range states {
				states[i].TargetTime = now.Add(time.Second)
			} // impossible goal still expresses usable intent
			svc := &Service{LoadpointStates: func() []loadpoint.State { return states }}
			d, ok := svc.SlotDirectiveAt(now)
			if !ok || !d.EVOnly || d.BatteryEnergyWh != 0 {
				t.Fatalf("no usable EV-only reserve: %+v ok=%v", d, ok)
			}
			if len(d.LoadpointFallback) != n {
				t.Fatalf("missing goal: %+v", d)
			}
			for _, st := range states {
				if d.LoadpointEnergyWh[st.ID] <= 0 || d.LoadpointMaxPowerW[st.ID] <= 0 {
					t.Fatalf("goal suppressed: %+v", d)
				}
			}
			if svc.Latest() != nil {
				t.Fatal("live goal reserve invented accepted plan")
			}
		})
	}
}

func TestGoalReserveKeepsKnownZeroAndIndependentCars(t *testing.T) {
	now := time.Now()
	a, b := deadlineGoalState(now, "a"), deadlineGoalState(now, "b")
	states := []loadpoint.State{a, b}
	svc := &Service{LoadpointStates: func() []loadpoint.State { return states }}
	original := SlotDirective{SlotStart: now, SlotEnd: now.Add(time.Hour), LoadpointEnergyWh: map[string]float64{"a": 0}, LoadpointMaxPowerW: map[string]float64{"a": 11000}}
	d, ok := svc.withDeadlineReserve(now, original, true)
	if !ok || d.EVOnly || d.LoadpointEnergyWh["a"] != 0 || d.LoadpointFallback["a"] {
		t.Fatalf("accepted cheap-future zero lost: %+v", d)
	}
	if d.LoadpointEnergyWh["b"] <= 0 || !d.LoadpointFallback["b"] {
		t.Fatalf("missing car goal not retained: %+v", d)
	}
	if _, mutated := original.LoadpointEnergyWh["b"]; mutated {
		t.Fatal("shared accepted plan map mutated")
	}
	states[0].CurrentSoC = math.NaN()
	d, ok = svc.SlotDirectiveAt(now)
	if !ok || d.LoadpointEnergyWh["b"] <= 0 {
		t.Fatalf("one bad car erased good car: %+v", d)
	}
	if _, bad := d.LoadpointEnergyWh["a"]; bad {
		t.Fatalf("NaN car got invented intent: %+v", d)
	}
}

func TestGoalReachedPauseAndMissingState(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name           string
		change         func(*loadpoint.State)
		wantZeroIntent bool
	}{
		{"goal reached", func(s *loadpoint.State) { s.CurrentSoC = .8 }, false},
		{"unplugged", func(s *loadpoint.State) { s.PluggedIn = false }, false},
		{"missing capacity", func(s *loadpoint.State) { s.VehicleCapacityWh = 0 }, false},
		{"NaN capacity", func(s *loadpoint.State) { s.VehicleCapacityWh = math.NaN() }, false},
		{"infinite max", func(s *loadpoint.State) { s.MaxChargeW = math.Inf(1) }, false},
		{"bad step", func(s *loadpoint.State) { s.AllowedStepsW = []float64{0, math.NaN(), 11000} }, false},
		{"no goal deadline", func(s *loadpoint.State) { s.TargetTime = time.Time{} }, false},
		{"surplus only", func(s *loadpoint.State) { s.SurplusOnly = true }, false},
		{"manual pause", func(s *loadpoint.State) { s.ManualActive = true; s.ManualChargeW = 0 }, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := deadlineGoalState(now, "a")
			tc.change(&st)
			svc := &Service{LoadpointStates: func() []loadpoint.State { return []loadpoint.State{st} }}
			d, ok := svc.SlotDirectiveAt(now)
			if tc.wantZeroIntent {
				if !ok || d.LoadpointEnergyWh["a"] != 0 || d.LoadpointMaxPowerW["a"] != 0 {
					t.Fatalf("manual pause lost: %+v ok=%v", d, ok)
				}
			} else if ok {
				t.Fatalf("invalid/completed state invented reserve: %+v", d)
			}
		})
	}
}

func TestDeadlineReservePreservesAcceptedLegacyZero(t *testing.T) {
	now := time.Now()
	st := deadlineGoalState(now, "a")
	svc := &Service{LoadpointStates: func() []loadpoint.State { return []loadpoint.State{st} }}
	svc.InstallPlan(Plan{GeneratedAtMs: now.UnixMilli(), Mode: ModeArbitrage,
		Actions: []Action{{SlotStartMs: now.UnixMilli(), SlotLenMin: 15, LoadpointW: 0}}}, Params{Mode: ModeArbitrage}, st.ID)
	d, ok := svc.SlotDirectiveAt(now)
	wh, hasBudget := d.LoadpointEnergyWh[st.ID]
	if !ok || !hasBudget || wh != 0 || d.LoadpointFallback[st.ID] || d.EVOnly {
		t.Fatalf("known cheap-future zero turned into a charge reserve: %+v", d)
	}
}

// A car whose own charger takes less than a stepless charger's minimum, such
// as a 3.7 kW plug-in hybrid on an 11 or 22 kW charger, gets the charger's
// minimum without a plan, as it does with one: the car takes what it can.
func TestGoalReserveGivesASmallCarTheChargersMinimum(t *testing.T) {
	now := time.Now()
	for _, maxW := range []float64{11040, 22080} {
		st := deadlineGoalState(now, "a")
		st.MinChargeW, st.MaxChargeW, st.AllowedStepsW, st.CarMaxW = 4140, maxW, nil, 3680
		svc := &Service{LoadpointStates: func() []loadpoint.State { return []loadpoint.State{st} }}
		d, ok := svc.SlotDirectiveAt(now)
		if !ok || !d.LoadpointFallback["a"] || d.LoadpointMaxPowerW["a"] != 4140 || d.LoadpointEnergyWh["a"] <= 0 {
			t.Fatalf("%.0f W charger: reserve %+v ok=%v, want 4140 W", maxW, d, ok)
		}
	}
}
