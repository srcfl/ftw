package main

import (
	"log/slog"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

// seedPlannerMode makes "planner on from the start" mean that the plan
// drives. A box that has never stored a control mode, with the planner on in
// its config, starts in the planner mode its battery-export answer maps to,
// and stores it like the other first-boot values. Without this it would run
// the default self_consumption and wait for a tap on "Use the plan".
//
// A stored mode is a choice already made, by the household or an earlier
// boot, and stays. Returns whether it seeded.
func seedPlannerMode(ctrl *control.State, st *state.Store, cfg *config.Config, export config.BatteryExport) bool {
	if ctrl == nil || st == nil || cfg == nil || cfg.Planner == nil || !cfg.Planner.Enabled {
		return false
	}
	if _, stored := st.LoadConfig("mode"); stored {
		return false
	}
	mode := control.Mode(export.PlannerModeKey())
	ctrl.Mode = mode
	if err := st.SaveConfig("mode", string(mode)); err != nil {
		slog.Warn("planner on from the start, but the mode could not be saved", "mode", mode, "err", err)
		return true
	}
	slog.Info("no stored control mode and the planner is on: starting in the planner mode", "mode", mode)
	return true
}
