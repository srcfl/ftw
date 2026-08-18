package control

import (
	"github.com/srcfl/ftw/go/internal/mpc"
)

// SlotDirectiveFromMPC is the plan→EMS bridge. main.go and the site
// clock must share it so EV energy budgets cannot drift from the
// battery slot the dispatcher executes.
func SlotDirectiveFromMPC(d mpc.SlotDirective) SlotDirective {
	return SlotDirective{
		DecisionID:             d.DecisionID,
		SlotStart:              d.SlotStart,
		SlotEnd:                d.SlotEnd,
		BatteryEnergyWh:        d.BatteryEnergyWh,
		SoCTargetPct:           d.SoCTargetPct,
		Strategy:               string(d.Strategy),
		PVLimitW:               d.PVLimitW,
		PlannedGridW:           d.GridW,
		HasPlannedGridW:        true,
		LivePVSurplusSoCCapPct: d.LivePVSurplusSoCCapPct,
		LoadpointEnergyWh:      d.LoadpointEnergyWh,
	}
}
