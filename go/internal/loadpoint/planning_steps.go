package loadpoint

import "math"

// PlanningSteps exposes legal whole-amp offers for a scheduled charge.
// Scheduled charging runs on the car's phases, three unless the loadpoint is
// pinned to one, as resolvePhaseMode sends it. Explicit device steps keep
// those at or above 6 A on those phases; one-phase levels in a shared table
// would let the plan spread a night at 3 kW that only one phase can deliver.
// Without such a step the whole-amp ladder stands in.
func PlanningSteps(cfg Config, site SiteFuse) []float64 {
	phases := site.Phases()
	if cfg.PhaseMode == "1p" {
		phases = 1
	}
	voltage := site.Voltage
	if voltage <= 0 {
		voltage = 230
	}
	step := voltage * float64(phases)
	if len(cfg.AllowedStepsW) > 0 {
		if phases == 1 || cfg.MaxChargeW < 6*step {
			return append([]float64(nil), cfg.AllowedStepsW...)
		}
		steps := []float64{0}
		for _, w := range cfg.AllowedStepsW {
			if w >= 6*step && (cfg.MaxChargeW <= 0 || w <= cfg.MaxChargeW) {
				steps = append(steps, w)
			}
		}
		if len(steps) > 1 {
			return steps
		}
	}
	minimum := math.Max(6, math.Ceil(cfg.MinChargeW/step))
	maximum := math.Floor(cfg.MaxChargeW / step)
	if site.MaxAmps > 0 {
		maximum = math.Min(maximum, math.Floor(site.MaxAmps))
	}
	steps := []float64{0}
	// IEC home charging is bounded here even if stored configuration is bad.
	if !finite(minimum) || !finite(maximum) || maximum > 1000 {
		return steps
	}
	for amps := minimum; amps <= maximum; amps++ {
		steps = append(steps, amps*step)
	}
	return steps
}
