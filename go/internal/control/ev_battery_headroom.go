package control

import (
	"math"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// clampEVBatteryHeadroom keeps optional battery charge out of room an EV
// still needs. It reads the site meter and our own commands. A battery's own
// reading only bounds how much of a discharge we ordered it may keep. A
// charger's reading counts only before any of its offers of this run has
// surely landed, and only up to what the meter shows (evFoundDrawW). The EV
// still needs its demand less the offers the meter surely shows (evShownW)
// and that draw.
func clampEVBatteryHeadroom(targets []DispatchTarget, store *telemetry.Store, state *State, fuseMaxW float64) []DispatchTarget {
	if store == nil || state == nil || len(targets) == 0 || fuseMaxW <= 0 {
		return targets
	}
	now := state.now()
	var demandW float64
	phases := 3.0 // No phase placement promise for 1p or auto chargers.
	for _, demand := range state.evFuseDemands {
		demandW += demand.powerW
		if demand.phaseMode != "3p" {
			phases = 1
		}
	}
	if len(state.evFuseDemands) == 0 {
		return targets
	}
	meter := store.Get(state.SiteMeterDriver, telemetry.DerMeter)
	if meter == nil {
		return floorPositiveEVBatteryTargets(targets)
	}
	gridW := meter.RawW
	if !finiteFuseValue(gridW) || !finiteFuseValue(fuseMaxW) {
		return floorPositiveEVBatteryTargets(targets)
	}
	offeredW := state.evShownW(now)
	consumptionW := gridW - sumOnlineSignedW(store, telemetry.DerBattery) - sumOnlineSignedW(store, telemetry.DerPV) - store.SumOnlineV2XW()
	offeredW += state.evFoundDrawW(store, now, consumptionW, offeredW)
	needW := math.Max(0, demandW-offeredW)
	latest := make(map[string]float64, len(targets))
	lowest := make(map[string]float64, len(targets))
	targeted := make(map[string]bool, len(targets))
	for _, target := range targets {
		if targeted[target.Driver] {
			return floorPositiveEVBatteryTargets(targets)
		}
		targeted[target.Driver] = true
		if _, _, last, sent := state.batteryCommands[target.Driver].span(now); sent {
			latest[target.Driver] = last
			// Only a discharge we ordered can be kept; a battery we have
			// not ordered keeps nothing.
			if r := store.Get(target.Driver, telemetry.DerBattery); r != nil {
				lowest[target.Driver], _ = fuseTargetBounds(r, state.DriverLimits[target.Driver])
			}
		} else {
			// Without a recent order it may be discharging and stop.
			latest[target.Driver] = -state.DriverLimits[target.Driver].dischargeCap()
		}
	}
	// Battery charge stays under the level the EV may rise to. Ending a
	// discharge needs room past our own raises the meter may not show, the
	// EV's included, not past what the car still wants.
	riseW := math.Max(0, state.EVCommandedW-offeredW)
	ceilingW := state.importRiseCeilingW(fuseMaxW) - gridW
	spare, releaseSpare := ceilingW-needW, ceilingW-riseW
	// During measured export overflow, the existing fuse pipeline may
	// require charge to reduce export; leave that direction to it. The
	// phases carry their direction (phaseCurrentsA): the worst phase is the
	// one importing most, and an exporting phase leaves the charge more room.
	if gridW >= -state.effectiveExportCeilingW(fuseMaxW) && state.SiteFuseVoltage > 0 {
		if phaseSpare, known := evBatteryPhaseSpareW(meter, state, gridW, needW/(state.SiteFuseVoltage*phases)); known {
			spare = math.Min(spare, phaseSpare)
		}
		if phaseSpare, known := evBatteryPhaseSpareW(meter, state, gridW, riseW/(state.SiteFuseVoltage*phases)); known {
			releaseSpare = math.Min(releaseSpare, phaseSpare)
		}
	}
	out := capBatteryCharge(targets, latest, lowest, state.pendingBatteryCommandsW(now), spare, releaseSpare)
	// Charge needed to bring measured export inside its hard ceiling is
	// not optional. Preserve that part of the already validated offer;
	// this stage never invents or increases an original battery command.
	var offeredCharge, finalCharge float64
	for i := range targets {
		offeredCharge += math.Max(0, targets[i].TargetW)
		finalCharge += math.Max(0, out[i].TargetW)
	}
	exportFloor := math.Min(offeredCharge, math.Max(0, -gridW-state.effectiveExportCeilingW(fuseMaxW)))
	if exportFloor > finalCharge && offeredCharge > finalCharge {
		scale := (exportFloor - finalCharge) / (offeredCharge - finalCharge)
		for i := range out {
			if targets[i].TargetW > out[i].TargetW && targets[i].TargetW > 0 {
				out[i].TargetW += (targets[i].TargetW - out[i].TargetW) * scale
			}
		}
	}
	return out
}

// capBatteryCharge keeps each battery's latest order: holding it is never an
// increase. A shortfall of measured room takes positive charge back; new
// charge needs room left after our increases the meter may not show yet. A
// reduction this tick funds nothing, as the meter does not show it. Ending a
// discharge is our increase too: the part releaseSpare, less our unshown
// increases, cannot take stays as discharge, never below lowest, the lowest
// order each battery's bounds allow (0 W without a reading).
func capBatteryCharge(targets []DispatchTarget, latest, lowest map[string]float64, pendingW, spare, releaseSpare float64) []DispatchTarget {
	if !finiteFuseValue(spare) || !finiteFuseValue(pendingW) {
		return floorPositiveEVBatteryTargets(targets)
	}
	out := append([]DispatchTarget(nil), targets...)
	holds := make([]float64, len(out))
	var holdW, increaseW, lostW float64
	for i, target := range out {
		last := latest[target.Driver]
		lost := math.Max(0, math.Min(0, target.TargetW)-math.Min(0, last)) // Lost discharge.
		spare, lostW = spare-lost, lostW+lost
		if target.TargetW > 0 {
			holds[i] = math.Min(target.TargetW, math.Max(0, last))
			holdW += holds[i]
			increaseW += target.TargetW - holds[i]
		}
	}
	holdScale, increaseScale := 1.0, 0.0
	if spare < 0 && holdW > 0 {
		holdScale = math.Max(0, 1+spare/holdW)
	}
	if room := spare - pendingW; room > 0 && increaseW > 0 {
		increaseScale = math.Min(1, room/increaseW)
	}
	for i := range out {
		if out[i].TargetW <= 0 {
			continue
		}
		capped := holds[i]*holdScale + (out[i].TargetW-holds[i])*increaseScale
		if capped < out[i].TargetW {
			out[i].TargetW, out[i].Clamped = capped, true
		}
	}
	return keepEndingDischarge(out, latest, lowest, lostW+pendingW-releaseSpare-holdW*(1-holdScale))
}

// keepEndingDischarge keeps shortW of the discharge this tick's targets end,
// shared by how much each battery can keep: down to its latest order, never
// below its lowest.
func keepEndingDischarge(out []DispatchTarget, latest, lowest map[string]float64, shortW float64) []DispatchTarget {
	keep := make([]float64, len(out))
	var keepW float64
	for i, target := range out {
		if last := latest[target.Driver]; last < 0 {
			keep[i] = math.Max(0, math.Min(0, target.TargetW)-math.Max(last, lowest[target.Driver]))
			keepW += keep[i]
		}
	}
	if !(shortW > 0) || keepW <= 0 {
		return out
	}
	share := math.Min(1, shortW/keepW)
	for i := range out {
		if keep[i] > 0 {
			out[i].TargetW, out[i].Clamped = math.Min(0, out[i].TargetW)-keep[i]*share, true
		}
	}
	return out
}

func evBatteryPhaseSpareW(meter *telemetry.DerReading, state *State, gridW, evIncreaseA float64) (float64, bool) {
	worst, present, valid := worstPhaseA(meter, state, gridW)
	if !present {
		return 0, false
	}
	if !valid {
		return math.Inf(-1), true
	}
	// The same working limit as the EV phase guard, with no floor under it.
	return (state.fuseLineA() - worst - evIncreaseA) * state.SiteFuseVoltage * float64(state.SiteFusePhases), true
}

func floorPositiveEVBatteryTargets(targets []DispatchTarget) []DispatchTarget {
	out := append([]DispatchTarget(nil), targets...)
	for i := range out {
		if out[i].TargetW > 0 {
			out[i].TargetW, out[i].Clamped = 0, true
		}
	}
	return out
}
