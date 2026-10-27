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

// Review of #1807: on a table with one-phase and three-phase levels the
// reserve holds to the limits the plan gets, as planned charging runs on
// three phases. A 3.7 kW car is reserved the lowest three-phase level, at
// full need and near its goal; marked one-phase, it is reserved the third of
// a three-phase level it takes. A one-phase level was ordered 0 W.
func TestGoalReserveHoldsToThePlansLimits(t *testing.T) {
	now := time.Now()
	steps := []float64{0}
	for _, phases := range []float64{1, 3} {
		for a := 6.0; a <= 16; a++ {
			steps = append(steps, a*230*phases)
		}
	}
	site := loadpoint.SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3}
	for _, c := range []struct {
		name      string
		carPhases int
		soc       float64
		want      float64
	}{
		{"a 3.7 kW car", 0, .3, 4140},
		{"a 3.7 kW car near its goal", 0, .7999, 4140},
		{"marked one-phase", 1, .3, 3680},
		{"marked one-phase near its goal", 1, .7999, 1380},
	} {
		st := deadlineGoalState(now, "a")
		st.MinChargeW, st.MaxChargeW, st.AllowedStepsW, st.CarMaxW, st.CarPhases, st.CurrentSoC = 1380, 11040, steps, 3680, c.carPhases, c.soc
		svc := &Service{
			LoadpointStates: func() []loadpoint.State { return []loadpoint.State{st} },
			PlanningLimits: func(st loadpoint.State) (float64, float64, []float64) {
				return loadpoint.PlanningLimits(st, loadpoint.Config{MinChargeW: st.MinChargeW, MaxChargeW: st.MaxChargeW, AllowedStepsW: st.AllowedStepsW}, site)
			},
		}
		d, ok := svc.SlotDirectiveAt(now)
		if !ok || !d.LoadpointFallback["a"] || d.LoadpointMaxPowerW["a"] != c.want || d.LoadpointEnergyWh["a"] <= 0 {
			t.Errorf("%s: reserve %v W, %v Wh, ok=%v; want %v W", c.name, d.LoadpointMaxPowerW["a"], d.LoadpointEnergyWh["a"], ok, c.want)
		}
	}
}

// srcfl/ftw#1861: Charge now at full power on a loadpoint pinned to 3p with
// a 3,800 W maximum runs at 4,140 W, 6 A on each phase. The reserve holds
// what Core orders, not the configured maximum.
func TestManualReserveHoldsTheLowestThreePhaseLevel(t *testing.T) {
	now := time.Now()
	cfg := loadpoint.Config{ID: "a", MinChargeW: 3800, MaxChargeW: 3800, AllowedStepsW: []float64{0, 3800}, PhaseMode: "3p"}
	site := loadpoint.SiteFuse{MaxAmps: 20, Voltage: 230, PhaseCnt: 3}
	for _, holdW := range []float64{3800, 4140} {
		st := deadlineGoalState(now, "a")
		st.MinChargeW, st.MaxChargeW, st.AllowedStepsW = cfg.MinChargeW, cfg.MaxChargeW, cfg.AllowedStepsW
		st.ManualActive, st.ManualChargeW = true, holdW
		svc := &Service{LoadpointStates: func() []loadpoint.State { return []loadpoint.State{st} },
			PlanningLimits: func(st loadpoint.State) (float64, float64, []float64) { return loadpoint.PlanningLimits(st, cfg, site) }}
		if d, ok := svc.SlotDirectiveAt(now); !ok || d.LoadpointMaxPowerW["a"] != 4140 {
			t.Errorf("Charge now %.0f W: reserve %+v ok=%v, want 4140 W", holdW, d, ok)
		}
	}
}
