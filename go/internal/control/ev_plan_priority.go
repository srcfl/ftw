package control

import "math"

// The active plan contains EV energy only for a connected scheduled goal.
// Keep its demand available while the charger ramps up, even before it draws.
// A duty-cycle pulse spends a small Wh budget at a legal on-power. Fuse
// sharing must reserve that on-power, not Wh/slot, or the battery can take
// headroom the charger is about to use.
func scheduledEVPowerW(state *State) float64 {
	if state == nil || !state.Mode.IsPlannerMode() {
		return 0
	}
	dir, ok := planDirectiveForIntent(state)
	if !ok || state.now().Before(dir.SlotStart) || !state.now().Before(dir.SlotEnd) {
		return 0
	}
	hours := dir.SlotEnd.Sub(dir.SlotStart).Hours()
	if hours <= 0 {
		return 0
	}
	var reserved float64
	for id, wh := range dir.LoadpointEnergyWh {
		if math.IsNaN(wh) || math.IsInf(wh, 0) || wh < 0 {
			return 0
		}
		if wh <= 0 {
			continue
		}
		if peak, ok := dir.LoadpointMaxPowerW[id]; ok {
			if math.IsNaN(peak) || math.IsInf(peak, 0) || peak < 0 {
				return 0
			}
			if peak > 0 {
				reserved += peak
				continue
			}
		}
		reserved += wh / hours
	}
	return reserved
}
