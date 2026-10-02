package mpc

import (
	"context"
	"testing"
	"time"
)

// #1497: after a departure passes while the car is still plugged in, the next
// one lies past the published prices. Both planners work toward it at the
// horizon's last slot; Energyplan used to reject the request instead.
func deadlinePastHorizon() ([]Slot, Params) {
	slots := []Slot{
		{StartMs: 0, LenMin: 60, PVW: -6000, LoadW: 500, PriceOre: 100, SpotOre: 30},
		{StartMs: 3600000, LenMin: 60, PVW: -6000, LoadW: 500, PriceOre: 100, SpotOre: 30},
		{StartMs: 7200000, LenMin: 60, LoadW: 500, PriceOre: 200, SpotOre: 60},
	}
	p := Params{Mode: ModeArbitrage, CapacityWh: 10000, InitialSoC: .5,
		SoCMin: .1, SoCMax: .9, ChargeEfficiency: 1, DischargeEfficiency: 1,
		Loadpoint: &LoadpointSpec{ID: "car", CapacityWh: 60000, Levels: 61,
			SoCMax: 1, InitialSoC: .5, TargetSoC: .8, TargetSlotIdx: 20, PluggedIn: true,
			SurplusOnly: true, ChargeEfficiency: 1, MaxChargeW: 11000, AllowedStepsW: []float64{0, 5500, 11000}}}
	return slots, p
}

func TestEVDeadlinePastHorizonRequestUsesLastSlot(t *testing.T) {
	slots, p := deadlinePastHorizon()
	req := (&ExternalOptimizer{}).buildRequest(slots, p)
	if got := req.FlexLoads[0].TargetSlot; got != len(slots)-1 {
		t.Fatalf("request deadline = %d, want the last slot %d", got, len(slots)-1)
	}
}

func TestNativeEVDeadlinePastHorizonPlans(t *testing.T) {
	slots, p := deadlinePastHorizon()
	worker := nativeWorker(t, 500*time.Millisecond)
	t.Cleanup(func() { _ = worker.Close() })
	plan, err := worker.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatalf("Energyplan rejected a deadline past the horizon: %v", err)
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Solver.Fallback || plan.Actions[0].LoadpointW <= 0 || plan.Actions[1].LoadpointW <= 0 {
		t.Fatalf("surplus did not go toward the departure: %+v", plan.Actions)
	}
	// The departure is planned when its prices arrive; until then nothing
	// can be missed, so the plan reports no shortfall.
	if len(plan.LoadpointShortfallWh) != 0 {
		t.Fatalf("a departure past the horizon was reported as missed: %v", plan.LoadpointShortfallWh)
	}
}

// chargerSteps are a charger's 6–16 A steps at the given line voltage sum.
func chargerSteps(volts float64) []float64 {
	steps := []float64{0}
	for amps := 6.0; amps <= 16; amps++ {
		steps = append(steps, amps*volts)
	}
	return steps
}

// A car that leaves inside the horizon goes first: a later departure must not
// take the surplus it needs. With the same deadline, the worker gave one hour
// of 5 kW surplus to tomorrow's three-phase car and left today's single-phase
// car 3 kWh short.
func departureAndNextDay() ([]Slot, Params) {
	slots := make([]Slot, 64)
	for i := range slots {
		slots[i] = Slot{StartMs: int64(i) * 900000, LenMin: 15, LoadW: 500, PriceOre: 150, SpotOre: 40}
		if i < 4 {
			slots[i].PVW = -5500
		}
	}
	p := Params{Mode: ModeArbitrage, CapacityWh: 10000, InitialSoC: .5,
		SoCMin: .1, SoCMax: .9, ChargeEfficiency: 1, DischargeEfficiency: 1,
		Loadpoints: []*LoadpointSpec{
			{ID: "today", CapacityWh: 60000, Levels: 61, SoCMax: 1, InitialSoC: .5, TargetSoC: .55,
				TargetSlotIdx: 63, PluggedIn: true, SurplusOnly: true, ChargeEfficiency: 1,
				MaxChargeW: 3680, AllowedStepsW: chargerSteps(230)},
			{ID: "tomorrow", CapacityWh: 60000, Levels: 61, SoCMax: 1, InitialSoC: .5, TargetSoC: .8,
				TargetSlotIdx: 160, PluggedIn: true, SurplusOnly: true, ChargeEfficiency: 1,
				MaxChargeW: 11040, AllowedStepsW: chargerSteps(690)},
		}}
	return slots, p
}

func TestEVDeadlineInsideHorizonGoesFirst(t *testing.T) {
	slots, p := departureAndNextDay()
	req := (&ExternalOptimizer{}).buildRequest(slots, p)
	if req.FlexLoads[0].TargetSlot != 63 || req.FlexLoads[1].TargetSlot != -1 {
		t.Fatalf("deadlines = %d and %d, want 63 for today's car and none for tomorrow's",
			req.FlexLoads[0].TargetSlot, req.FlexLoads[1].TargetSlot)
	}
}

func TestNativeEVDeadlineInsideHorizonGoesFirst(t *testing.T) {
	slots, p := departureAndNextDay()
	worker := nativeWorker(t, 500*time.Millisecond)
	t.Cleanup(func() { _ = worker.Close() })
	plan, err := worker.Optimize(context.Background(), slots, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePlan(slots, p, &plan); err != nil {
		t.Fatal(err)
	}
	if plan.LoadpointShortfallWh["today"] > 0 {
		t.Fatalf("today's departure lost its surplus to tomorrow's car: %v", plan.LoadpointShortfallWh)
	}
}

func TestEVDeadlineSlot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target float64
		slot   int
		want   int
	}{
		{"inside", .8, 1, 1},
		{"past the horizon", .8, 20, 2},
		{"no target", 0, 20, -1},
		{"no deadline", .8, -1, -1},
	} {
		lp := &LoadpointSpec{TargetSoC: tc.target, TargetSlotIdx: tc.slot}
		if got := lp.deadlineSlot(3); got != tc.want {
			t.Errorf("%s: deadlineSlot = %d, want %d", tc.name, got, tc.want)
		}
	}
}
