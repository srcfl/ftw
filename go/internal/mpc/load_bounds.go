package mpc

import "math"

// sanitizeLoadW maps a NaN, infinite or negative house-load estimate to 0 W.
// Load is consumption, so a value below zero is a bad reading, not export.
func sanitizeLoadW(w float64) float64 {
	if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
		return 0
	}
	return w
}

func sanitizeSlotLoads(slots []Slot) {
	for i := range slots {
		slots[i].LoadW = sanitizeLoadW(slots[i].LoadW)
	}
}

func sanitizePlanLoads(plan *Plan) {
	if plan == nil {
		return
	}
	for i := range plan.Actions {
		plan.Actions[i].LoadW = sanitizeLoadW(plan.Actions[i].LoadW)
	}
}
