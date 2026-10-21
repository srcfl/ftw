package control

import (
	"math"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

type evFuseDemand struct {
	driverName string
	powerW     float64
	phaseMode  string
}

// SetEVFuseDemands copies the charger decisions made before the fuse clamp.
// Callers replace the snapshot each tick while holding the control lock.
func (state *State) SetEVFuseDemands(demands []loadpoint.FuseDemand) {
	state.evFuseDemandsSet = true
	state.evFuseDemands = nil
	byDriver := make(map[string]int, len(demands))
	for _, demand := range demands {
		if demand.DriverName == "" || !finiteFuseValue(demand.PowerW) || demand.PowerW <= 0 {
			continue
		}
		phase := demand.PhaseMode
		if phase != "1p" && phase != "2p" && phase != "3p" {
			phase = "auto"
		}
		if i, ok := byDriver[demand.DriverName]; ok {
			state.evFuseDemands[i].powerW = math.Max(state.evFuseDemands[i].powerW, demand.PowerW)
			if state.evFuseDemands[i].phaseMode != phase {
				state.evFuseDemands[i].phaseMode = "auto"
			}
			continue
		}
		byDriver[demand.DriverName] = len(state.evFuseDemands)
		state.evFuseDemands = append(state.evFuseDemands, evFuseDemand{demand.DriverName, demand.PowerW, phase})
	}
}

// The active plan contains EV energy only for a connected scheduled goal.
// Keep its demand available while the charger ramps up, even before it draws.
// A duty-cycle pulse spends a small Wh budget at a legal on-power. Fuse
// sharing must reserve that on-power, not Wh/slot, or the battery can take
// headroom the charger is about to use.
func scheduledEVPowerW(state *State) float64 {
	if state == nil {
		return 0
	}
	commanded := state.EVCommandedW
	if math.IsNaN(commanded) || math.IsInf(commanded, 0) || commanded < 0 {
		commanded = 0
	}
	var requested float64
	for _, demand := range state.evFuseDemands {
		requested += demand.powerW
	}
	commanded = math.Max(commanded, requested)
	// A live charger decision supersedes an earlier plan, including an
	// empty snapshot after Stop, a completed goal or a duty-cycle pause.
	if state.evFuseDemandsSet {
		return commanded
	}
	if !state.Mode.IsPlannerMode() {
		return commanded
	}
	dir, ok := planDirectiveForIntent(state)
	// EV reserve intent remains available while the battery controller
	// honestly follows its missing-plan self-consumption path.
	if !ok && state.controlPlanSnapshot.directiveOK && state.controlPlanSnapshot.directive.EVOnly {
		dir, ok = state.controlPlanSnapshot.directive, true
	}
	if !ok || state.now().Before(dir.SlotStart) || !state.now().Before(dir.SlotEnd) {
		return commanded
	}
	hours := dir.SlotEnd.Sub(dir.SlotStart).Hours()
	if hours <= 0 {
		return commanded
	}
	var reserved float64
	for id, wh := range dir.LoadpointEnergyWh {
		if math.IsNaN(wh) || math.IsInf(wh, 0) || wh < 0 {
			return commanded
		}
		if wh <= 0 {
			continue
		}
		if peak, ok := dir.LoadpointMaxPowerW[id]; ok {
			if math.IsNaN(peak) || math.IsInf(peak, 0) || peak < 0 {
				return commanded
			}
			if peak > 0 {
				reserved += peak
				continue
			}
		}
		reserved += wh / hours
	}
	return math.Max(commanded, reserved)
}
