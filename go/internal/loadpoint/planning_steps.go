package loadpoint

import "math"

// PlanningSteps exposes legal whole-amp offers for a scheduled charge.
// Scheduled charging runs on the car's phases, as resolvePhaseMode sends it:
// three on a charger that can reach 6 A on each, else one. A three-phase
// charger's explicit steps keep those at or above 6 A on its phases; one-
// phase levels in a shared table would let the plan spread a night at 3 kW
// that only one phase can deliver. Other explicit steps keep their meaning.
func PlanningSteps(cfg Config, site SiteFuse) []float64 {
	phases := site.Phases()
	if !threePhaseCapable(cfg, site) {
		phases = 1
	}
	voltage := site.Voltage
	if voltage <= 0 {
		voltage = 230
	}
	step := voltage * float64(phases)
	if len(cfg.AllowedStepsW) > 0 {
		if phases == 1 {
			return append([]float64(nil), cfg.AllowedStepsW...)
		}
		steps := []float64{0}
		for _, w := range cfg.AllowedStepsW {
			if w >= 6*step && (cfg.MaxChargeW <= 0 || w <= cfg.MaxChargeW) {
				steps = append(steps, w)
			}
		}
		return steps
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

// threePhaseCapable reports whether a charger can run three phases at 6 A
// each: a three-phase site, no 1p pin, and a step table, or else a maximum,
// that reaches 6 A on every phase. A table with nothing that high is a
// one-phase charger whatever its mode says. A one-phase charger above 16 A
// with such a table needs phase_mode 1p.
func threePhaseCapable(cfg Config, site SiteFuse) bool {
	if site.Phases() != 3 || cfg.PhaseMode == "1p" {
		return false
	}
	voltage := site.Voltage
	if !finite(voltage) || voltage <= 0 {
		voltage = 230
	}
	minimum := 18 * voltage
	if finite(cfg.MaxChargeW) && cfg.MaxChargeW > 0 && cfg.MaxChargeW < minimum {
		return false
	}
	if len(cfg.AllowedStepsW) == 0 {
		return true
	}
	for _, w := range cfg.AllowedStepsW {
		if w >= minimum {
			return true
		}
	}
	return false
}
