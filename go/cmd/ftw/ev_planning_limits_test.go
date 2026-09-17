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
