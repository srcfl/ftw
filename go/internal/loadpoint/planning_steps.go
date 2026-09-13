package loadpoint

import "math"

// PlanningSteps exposes legal whole-amp offers for a scheduled charge when
// no explicit device steps were configured. Scheduled auto mode uses three
// phases, as resolvePhaseMode does. Explicit device steps keep their meaning.
func PlanningSteps(cfg Config, site SiteFuse) []float64 {
	if len(cfg.AllowedStepsW) > 0 {
		return append([]float64(nil), cfg.AllowedStepsW...)
	}
	phases := site.Phases()
	if cfg.PhaseMode == "1p" {
		phases = 1
	}
	voltage := site.Voltage
	if voltage <= 0 {
		voltage = 230
	}
	step := voltage * float64(phases)
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
