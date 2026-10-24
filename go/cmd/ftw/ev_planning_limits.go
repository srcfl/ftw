package main

import (
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// planningLimitsForLoadpoint is the power range and the steps the planner
// gets for a loadpoint's session.
func planningLimitsForLoadpoint(st loadpoint.State, cfg *config.Config) (minW, maxW float64, steps []float64) {
	lp := loadpoint.Config{MinChargeW: st.MinChargeW, MaxChargeW: st.MaxChargeW, AllowedStepsW: st.AllowedStepsW}
	for _, configured := range cfg.Loadpoints {
		if configured.ID == st.ID {
			lp.PhaseMode = configured.PhaseMode
			break
		}
	}
	site := loadpoint.SiteFuse{MaxAmps: cfg.Fuse.MaxAmps, Voltage: cfg.Fuse.Voltage, PhaseCnt: cfg.Fuse.Phases}
	return loadpoint.PlanningLimits(st, lp, site)
}
