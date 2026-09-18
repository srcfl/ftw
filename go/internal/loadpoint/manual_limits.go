package loadpoint

import "math"

// A manual selection is a ceiling. Never round it up to a larger step or
// let it bypass the configured charger's rating.
func clampManualPower(cfg Config, hold ManualHold, site SiteFuse) float64 {
	floor := cfg.MinChargeW
	mode := hold.PhaseMode
	if mode == "" {
		mode = cfg.PhaseMode
	}
	if (mode == "1p" || mode == "auto") && site.Phases() == 3 {
		floor /= 3
	}
	return floorChargeW(hold.PowerW, floor, cfg.MaxChargeW, cfg.AllowedStepsW)
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
