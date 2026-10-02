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
	// Two hours of surplus cannot add the 18 kWh the target needs; the plan
	// says how much is left at the end of the horizon.
	if plan.LoadpointShortfallWh["car"] <= 0 {
		t.Fatalf("shortfall at the horizon's end not reported: %v", plan.LoadpointShortfallWh)
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
