package loadpoint

import "math"

// SnapChargeW picks the nearest feasible step. want <= 0 is off, not
// the minimum step. Nearest, not floor: float error just under a step
// must still hit that step.
func SnapChargeW(want, min, max float64, steps []float64) float64 {
	if math.IsNaN(want) || math.IsInf(want, 0) || want <= 0 {
		return 0
	}
	if want < min {
		want = min
	}
	if max > 0 && want > max {
		want = max
	}
	if max > 0 && max < min {
		return 0
	}
	if len(steps) == 0 {
		return want
	}
	best := 0.0
	bestDiff := math.Inf(1)
	for _, step := range steps {
		if math.IsNaN(step) || math.IsInf(step, 0) || step < min || (max > 0 && step > max) {
			continue
		}
		if d := math.Abs(want - step); d < bestDiff {
			best = step
			bestDiff = d
		}
	}
	return best
}

// floorChargeW treats want as a hard ceiling. No feasible step means pause.
func floorChargeW(want, min, max float64, steps []float64) float64 {
	if math.IsNaN(want) || math.IsInf(want, 0) || want <= 0 {
		return 0
	}
	if max > 0 && want > max {
		want = max
	}
	if want < min {
		return 0
	}
	if len(steps) == 0 {
		return want
	}
	best := 0.0
	for _, step := range steps {
		if step >= min && step <= want && step > best {
			best = step
		}
	}
	return best
}

// PhaseFor maps mode and splitW to 1 or 3. Unknown modes stay on 3.
// splitW must come from the site, not a hard-coded 230 V. Drivers own
// the decision this function documents.
func PhaseFor(mode string, wantW, splitW float64) int {
	switch mode {
	case "1p":
		return 1
	case "auto":
		if splitW > 0 && wantW < splitW {
			return 1
		}
		return 3
	default: // "", "3p"
		return 3
	}
}

// FilterStepsByPhase keeps steps for phases. 0 stays. step <= splitW is 1Φ.
func FilterStepsByPhase(steps []float64, phases int, splitW float64) []float64 {
	if len(steps) == 0 {
		return nil
	}
	if splitW <= 0 {
		splitW = 3680 // 16 A × 230 V when the caller has no site split
	}
	out := make([]float64, 0, len(steps))
	out = append(out, 0)
	for _, s := range steps {
		if s <= 0 {
			continue
		}
		if phases == 1 && s <= splitW {
			out = append(out, s)
		} else if phases == 3 && s > splitW {
			out = append(out, s)
		}
	}
	return out
}

// EnergyBudgetToPowerW is remainingWh × 3600 / remainingS.
// A spent budget or a closed window is 0.
func EnergyBudgetToPowerW(remainingWh, remainingS float64) float64 {
	if remainingWh <= 0 {
		return 0
	}
	if remainingS <= 0 {
		return 0
	}
	return remainingWh * 3600.0 / remainingS
}
