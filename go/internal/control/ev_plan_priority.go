package control

import "math"

// The active plan contains EV energy only for a connected scheduled goal.
// Keep its demand available while the charger ramps up, even before it draws.
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
	var energy float64
	for _, wh := range dir.LoadpointEnergyWh {
		if math.IsNaN(wh) || math.IsInf(wh, 0) || wh < 0 {
			return 0
		}
		if wh > 0 {
			energy += wh
		}
	}
	return energy / hours
}
