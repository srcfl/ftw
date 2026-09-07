package control

import (
	"github.com/srcfl/ftw/go/internal/mpc"
	"maps"
)

// SlotDirectiveFromMPC is the plan→EMS bridge. main.go and the site
// clock must share it so EV energy budgets cannot drift from the
// battery slot the dispatcher executes.
func SlotDirectiveFromMPC(d mpc.SlotDirective) SlotDirective {
	return SlotDirective{
		DecisionID:          d.DecisionID,
		SlotStart:           d.SlotStart,
		SlotEnd:             d.SlotEnd,
		BatteryEnergyWh:     d.BatteryEnergyWh,
		StorageEnergyWh:     maps.Clone(d.StorageEnergyWh),
		SoCTarget:           d.SoCTarget,
		Strategy:            string(d.Strategy),
		PVLimitW:            d.PVLimitW,
		PVCurtailActive:     d.PVCurtailActive,
		PVCurtailment:       d.PVCurtailment,
		PlannedGridW:        d.GridW,
		HasPlannedGridW:     true,
		LivePVSurplusSoCCap: d.LivePVSurplusSoCCap,
		LoadpointEnergyWh:   maps.Clone(d.LoadpointEnergyWh),
	}
}
