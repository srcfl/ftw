package main

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
	"testing"
)

func TestConfiguredEVLimitsReachPlanner(t *testing.T) {
	cfg := &config.Config{Fuse: config.Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}, Loadpoints: []config.Loadpoint{{ID: "garage", PhaseMode: "1p"}}}
	st := loadpoint.State{ID: "garage", MinChargeW: 1380, MaxChargeW: 3680}
	steps := planningStepsForLoadpoint(st, cfg)
	if len(steps) != 12 || steps[1] != 1380 || steps[len(steps)-1] != 3680 {
		t.Fatalf("1-phase limits lost on 3-phase site: %v", steps)
	}
	cfg.Loadpoints[0].PhaseMode = "3p"
	st.MinChargeW = 4140
	st.MaxChargeW = 11000
	steps = planningStepsForLoadpoint(st, cfg)
	if steps[1] != 4140 || steps[len(steps)-1] != 10350 {
		t.Fatalf("3-phase limits lost: %v", steps)
	}
}

func TestTwoPhaseCarLimitsReachPlanner(t *testing.T) {
	m := loadpoint.NewManager()
	m.Load([]loadpoint.Config{{ID: "garage", MinChargeW: 4140, MaxChargeW: 11000, PhaseMode: "3p", AllowedStepsW: []float64{0, 4140, 6900, 11000}}})
	m.Observe("garage", true, 0, 0, true)
	m.SetCarBinding("garage", loadpoint.CarBinding{ID: "volvo", Phases: 2, MaxW: 6400})
	st, _ := m.State("garage")
	cfg := &config.Config{Fuse: config.Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}, Loadpoints: []config.Loadpoint{{ID: "garage", PhaseMode: "3p"}}}
	steps := planningStepsForLoadpoint(st, cfg)
	if len(steps) != 9 || steps[1] != 2760 || steps[len(steps)-1] != 5980 || st.MinChargeW != 2760 || st.MaxChargeW != 6400 {
		t.Fatalf("two-phase car lost at the planner: min=%v max=%v steps=%v", st.MinChargeW, st.MaxChargeW, steps)
	}
}
