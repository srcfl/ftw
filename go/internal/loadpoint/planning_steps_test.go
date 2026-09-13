package loadpoint

import (
	"slices"
	"testing"
)

func TestPlanningStepsRetainHouseHeadroom(t *testing.T) {
	cfg := Config{MinChargeW: 4140, MaxChargeW: 11000, PhaseMode: "3p"}
	got := PlanningSteps(cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	if !slices.Equal(got, []float64{0, 4140, 4830, 5520, 6210, 6900, 7590, 8280, 8970, 9660, 10350}) {
		t.Fatalf("three-phase steps %v", got)
	}
	cfg = Config{MinChargeW: 1380, MaxChargeW: 3680, PhaseMode: "1p"}
	got = PlanningSteps(cfg, SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	if got[1] != 1380 || got[len(got)-1] != 3680 {
		t.Fatalf("one-phase steps %v", got)
	}
	cfg.AllowedStepsW = []float64{0, 1400, 2300}
	got = PlanningSteps(cfg, SiteFuse{})
	got[1] = 0
	if cfg.AllowedStepsW[1] != 1400 {
		t.Fatal("mutated device steps")
	}
}
