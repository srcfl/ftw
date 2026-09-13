package mpc

import (
	"context"
	"testing"
	"time"
)

func TestRestoredEVLevelTriggersReplanWithoutPVDivergence(t *testing.T) {
	s, _ := buildTestService(t, 0, 500)
	s.PVDivergenceWh, s.LoadDivergenceWh = 0, 0
	old := &LoadpointSpec{ID: "garage", Levels: 11, SoCMax: 1, CapacityWh: 75000, InitialSoC: .33, PluggedIn: true, TargetSoC: .8, TargetSlotIdx: 3, MaxChargeW: 11000, ChargeEfficiency: .9}
	s.lastParams.Loadpoints = []*LoadpointSpec{old}
	current := *old
	current.InitialSoC = .776
	s.Loadpoints = func(int) []*LoadpointSpec { return []*LoadpointSpec{&current} }
	s.checkDivergence(context.Background())
	if s.lastReason != "loadpoint_soc_changed" || len(s.lastParams.Loadpoints) != 1 || s.lastParams.Loadpoints[0].InitialSoC != .776 {
		t.Fatalf("restoration did not reach planner: reason=%s params=%+v", s.lastReason, s.lastParams.Loadpoints)
	}
}

func TestEVLevelDivergenceAccountsForPlannedDelivery(t *testing.T) {
	start := time.Now().Truncate(time.Minute)
	initial := &LoadpointSpec{ID: "garage", Levels: 11, SoCMax: 1, CapacityWh: 60000, InitialSoC: .50, PluggedIn: true, TargetSoC: .8, MaxChargeW: 11000, ChargeEfficiency: .9}
	current := *initial
	current.InitialSoC += 11000 * .25 * .9 / 60000
	s := &Service{Loadpoints: func(int) []*LoadpointSpec { return []*LoadpointSpec{&current} }}
	p := Params{Loadpoints: []*LoadpointSpec{initial}}
	plan := &Plan{Actions: []Action{{SlotStartMs: start.UnixMilli(), SlotLenMin: 15, LoadpointPowerW: map[string]float64{"garage": 11000}}}}
	if s.loadpointStateDiverged(plan, p, start.Add(15*time.Minute)) {
		t.Fatal("normal planned delivery caused another replan")
	}
	current.InitialSoC += .01
	if s.loadpointStateDiverged(plan, p, start.Add(15*time.Minute)) {
		t.Fatal("small meter error caused another replan")
	}
	current.InitialSoC += .03
	if !s.loadpointStateDiverged(plan, p, start.Add(15*time.Minute)) {
		t.Fatal("larger correction did not trigger replan")
	}
}

func TestEVLevelDivergenceAcceptsDutyExecutionRange(t *testing.T) {
	start := time.Now().Truncate(time.Hour)
	initial := &LoadpointSpec{ID: "garage", Levels: 11, SoCMax: 1, CapacityWh: 60000, InitialSoC: .5, PluggedIn: true, TargetSoC: .8, MaxChargeW: 11000, ChargeEfficiency: .9}
	current := *initial
	s := &Service{Loadpoints: func(int) []*LoadpointSpec { return []*LoadpointSpec{&current} }}
	p := Params{Loadpoints: []*LoadpointSpec{initial}}
	for _, offset := range []time.Duration{0, 30 * time.Minute} {
		a := Action{SlotStartMs: start.UnixMilli(), ExecutionStartMs: start.Add(offset).UnixMilli(), SlotLenMin: 60, LoadpointPowerW: map[string]float64{"garage": 5500}, LoadpointMaxPowerW: map[string]float64{"garage": 11000}}
		plan := &Plan{Actions: []Action{a}}
		for _, fraction := range []float64{0, .25, .5, .75, 1} {
			duration := time.Hour - offset
			elapsed := time.Duration(float64(duration) * fraction)
			budget := 5500 * duration.Hours()
			front := min(budget, 11000*elapsed.Hours())
			back := max(0, budget-11000*(duration-elapsed).Hours())
			for _, wh := range []float64{front, back} {
				current.InitialSoC = .5 + wh*.9/60000
				if s.loadpointStateDiverged(plan, p, start.Add(offset+elapsed)) {
					t.Fatalf("legal duty charge triggered replan: offset=%v elapsed=%v Wh=%v", offset, elapsed, wh)
				}
			}
			current.InitialSoC = .5 + front*.9/60000 + .03
			if !s.loadpointStateDiverged(plan, p, start.Add(offset+elapsed)) {
				t.Fatal("correction outside the duty range did not replan")
			}
		}
	}
}
