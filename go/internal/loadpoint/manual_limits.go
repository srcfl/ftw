package loadpoint

import "math"

// A manual selection is a ceiling. Never round it up to a larger step or
// let it bypass the configured charger's rating.
func clampManualPower(cfg Config, hold ManualHold, site SiteFuse) float64 {
	minimum := manualMinChargeW(cfg, hold, site)
	w := floorChargeW(hold.PowerW, minimum, cfg.MaxChargeW, cfg.AllowedStepsW)
	amps := hold.MaxAmpsPerPhase
	if site.MaxAmps > 0 && (!finite(amps) || amps <= 0 || amps > site.MaxAmps) {
		amps = site.MaxAmps
	}
	if amps <= 0 {
		return w
	}
	mode := hold.PhaseMode
	if mode == "" {
		mode = cfg.PhaseMode
	}
	if site.Phases() == 1 {
		mode = "1p"
	} else if mode == "" && cfg.SurplusOnly {
		mode = "auto"
	}
	voltage := site.Voltage
	if voltage <= 0 {
		voltage = hold.Voltage
	}
	split := hold.PhaseSplitW
	if split <= 0 {
		split = cfg.PhaseSplitW
	}
	cmd := map[string]any{"power_w": w, "phase_mode": mode, "voltage": voltage, "phase_split_w": split, "max_amps_per_phase": amps}
	// A current reduction may move an auto offer from three phases to one.
	// Apply the same final ceiling until that phase choice is stable.
	for applyCurrentCeiling(cmd) {
	}
	return floorChargeW(cmd["power_w"].(float64), minimum, cfg.MaxChargeW, cfg.AllowedStepsW)
}

func manualMinChargeW(cfg Config, hold ManualHold, site SiteFuse) float64 {
	mode := hold.PhaseMode
	if mode == "" {
		mode = cfg.PhaseMode
	}
	if mode == "1p" || mode == "auto" {
		return phaseMinChargeW(cfg, mode, site)
	}
	return cfg.MinChargeW
}

// phaseMinChargeW is the smallest legal offer on the phases a command names:
// 6 A on one phase for 1p or auto, 6 A on each phase for 3p or empty.
func phaseMinChargeW(cfg Config, mode string, site SiteFuse) float64 {
	if site.Phases() != 3 {
		return cfg.MinChargeW
	}
	voltage := site.Voltage
	if !finite(voltage) || voltage <= 0 {
		voltage = 230
	}
	if mode == "1p" || mode == "auto" {
		return math.Max(cfg.MinChargeW/3, 6*voltage)
	}
	return math.Max(cfg.MinChargeW, 18*voltage)
}

func (c *Controller) applyInstallationLimits(cmd map[string]any) {
	site := c.siteFuse()
	if site.Voltage > 0 {
		cmd["voltage"] = site.Voltage
	}
	if site.PhaseCnt > 0 {
		cmd["site_phases"] = site.Phases()
	}
	if site.Phases() == 1 {
		cmd["phase_mode"] = "1p"
	}
	if site.MaxAmps > 0 {
		limit, _ := cmd["max_amps_per_phase"].(float64)
		if limit <= 0 || math.IsNaN(limit) || math.IsInf(limit, 0) || limit > site.MaxAmps {
			cmd["max_amps_per_phase"] = site.MaxAmps
		}
	}
}

// The watt order must reflect the final current ceiling too. In particular,
// drivers often interpret max_amps_per_phase=0 as an absent override; send an
// explicit zero-power order when the fuse leaves less than the 6 A minimum.
func applyCurrentCeiling(cmd map[string]any) bool {
	w, _ := cmd["power_w"].(float64)
	a, known := cmd["max_amps_per_phase"].(float64)
	if w <= 0 || !known {
		return false
	}
	if a < 6 {
		cmd["power_w"] = float64(0)
		return true
	}
	v, _ := cmd["voltage"].(float64)
	if v <= 0 {
		return false
	}
	mode, _ := cmd["phase_mode"].(string)
	split, _ := cmd["phase_split_w"].(float64)
	if split <= 0 {
		split = v * a
	}
	// A three-phase switch cannot work below 6 A on every phase.
	if split < 18*v {
		split = 18 * v
	}
	phases := PhaseFor(mode, w, split)
	ceiling := v * float64(phases) * math.Floor(a)
	if w > ceiling {
		cmd["power_w"] = ceiling
		return true
	}
	return false
}
