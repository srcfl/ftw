package loadpoint

import (
	"math"
	"slices"
)

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

// PlanningLimits is the power range and the steps the planner gets for a
// loadpoint's session, held to the car by LimitPower. A one-phase car on a
// three-phase charger takes a third of each three-phase order, so its
// levels are the charger's three-phase steps divided by three, up to its
// one-phase maximum: each is a third of an order the charger's table and
// minimum allow. The charger's own minimum would leave the planner no level.
func PlanningLimits(st State, cfg Config, site SiteFuse) (minW, maxW float64, steps []float64) {
	planning := PlanningSteps(cfg, site)
	if st.CarPhases == 1 && threePhaseCapable(cfg, site) {
		topW := OnePhaseCarMaxW(cfg, site)
		levels := []float64{0}
		for _, w := range planning {
			if w > 0 && w/3 <= topW+1e-9 {
				levels = append(levels, w/3)
			}
		}
		if len(levels) > 1 {
			st.MinChargeW, st.MaxChargeW = slices.Min(levels[1:]), slices.Max(levels)
			return LimitPower(st, levels)
		}
	}
	return LimitPower(st, planning)
}

// PlanningLimits is PlanningLimits on this loadpoint's charger and the site
// fuse, as both stand now. The deadline reserve holds to them, as the plan
// does. Without a controller the reserve holds to the charger's own table.
func (c *Controller) PlanningLimits(st State) (minW, maxW float64, steps []float64) {
	if c == nil {
		return LimitPower(st, st.AllowedStepsW)
	}
	cfg := Config{MinChargeW: st.MinChargeW, MaxChargeW: st.MaxChargeW, AllowedStepsW: st.AllowedStepsW}
	for _, lp := range c.manager.Configs() {
		if lp.ID == st.ID {
			cfg.PhaseMode = lp.PhaseMode
		}
	}
	return PlanningLimits(st, cfg, c.siteFuse())
}

// ThreePhaseCapable reports whether a charger can run three phases at 6 A
// each; see threePhaseCapable.
func ThreePhaseCapable(cfg Config, site SiteFuse) bool { return threePhaseCapable(cfg, site) }

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
