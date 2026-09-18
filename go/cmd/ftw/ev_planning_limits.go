package main

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
)

func planningStepsForLoadpoint(st loadpoint.State, cfg *config.Config) []float64 {
	lp := loadpoint.Config{MinChargeW: st.MinChargeW, MaxChargeW: st.MaxChargeW, AllowedStepsW: st.AllowedStepsW}
	for _, configured := range cfg.Loadpoints {
		if configured.ID == st.ID {
			lp.PhaseMode = configured.PhaseMode
			break
		}
	}
	return loadpoint.PlanningSteps(lp, loadpoint.SiteFuse{MaxAmps: cfg.Fuse.MaxAmps, Voltage: cfg.Fuse.Voltage, PhaseCnt: cfg.Fuse.Phases})
}
