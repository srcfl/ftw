package main

import (
	"log/slog"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

// isFirstBoot reports whether this is the box's very first start. Every
// start of Core records a "startup" event before it does anything else, and
// nothing prunes the event log; the setup wizard that runs before the first
// start writes no events. So an empty log, read before this start records
// its own, is a new box. A box that ran before, or was restored from a
// backup, has events. A read error counts as not new.
func isFirstBoot(st *state.Store) bool {
	if st == nil {
		return false
	}
	events, err := st.RecentEvents(1)
	return err == nil && len(events) == 0
}

// seedPlannerMode makes "planner on from the start" mean that the plan
// drives. On its very first start, a box with the planner on in its config
// and no stored control mode starts in the planner mode its battery-export
// answer maps to, and stores it like the other first-boot values. Without
// this it would run the default self_consumption and wait for a tap on
// "Use the plan".
//
// Only a new box: an existing box that never stored a mode has run
// self_consumption all along, and an update must not hand it to the
// planner without its owner. A stored mode always stays. Returns whether it
// seeded.
func seedPlannerMode(ctrl *control.State, st *state.Store, cfg *config.Config, export config.BatteryExport, firstBoot bool) bool {
	if !firstBoot || ctrl == nil || st == nil || cfg == nil || cfg.Planner == nil || !cfg.Planner.Enabled {
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
	slog.Info("new box with the planner on: starting in the planner mode", "mode", mode)
	return true
}
