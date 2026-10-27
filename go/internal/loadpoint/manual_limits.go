package loadpoint

import (
	"math"
	"slices"
)

// A manual selection is a ceiling. Never round it up to a larger step or
// let it bypass the configured charger's rating. The app's full power is
// the configured maximum: on phases whose minimum is above it, full power
// is that minimum. The reason names what held the order below the hold:
// below_phase_minimum when the hold is under the lowest level its phases
// can run, charger_limit for any other limit.
func clampManualPower(cfg Config, hold ManualHold, site SiteFuse) (float64, string) {
	mode := holdPhaseMode(hold, cfg, site)
	limits := phaseLimits(cfg, mode, site)
	ceiling := hold.PowerW
	if cfg.MaxChargeW > 0 && ceiling >= cfg.MaxChargeW {
		ceiling = math.Max(ceiling, limits.MaxChargeW)
	}
	w := floorChargeW(ceiling, limits.MinChargeW, limits.MaxChargeW, limits.AllowedStepsW)
	amps := hold.MaxAmpsPerPhase
	if site.MaxAmps > 0 && (!finite(amps) || amps <= 0 || amps > site.MaxAmps) {
		amps = site.MaxAmps
	}
	if amps > 0 {
		w = manualCurrentCeiling(w, amps, mode, cfg, hold, site)
		w = floorChargeW(w, limits.MinChargeW, limits.MaxChargeW, limits.AllowedStepsW)
	}
	switch {
	case w >= hold.PowerW:
		return w, ""
	case w == 0 && ceiling < limits.MinChargeW:
		return 0, "below_phase_minimum"
	}
	return w, "charger_limit"
}

// manualCurrentCeiling is w under the hold's current ceiling, as the
// command's own check will apply it.
func manualCurrentCeiling(w, amps float64, mode string, cfg Config, hold ManualHold, site SiteFuse) float64 {
	if site.Phases() == 1 {
		mode = "1p"
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
	return cmd["power_w"].(float64)
}

// phaseLimits holds cfg to the orders a command on mode's phases can run:
// at least 6 A on each phase. A maximum below that cannot be met on those
// phases, so the order runs at the minimum, the lowest level the charger
// has there. So does a step table with no level in reach. Startup and save
// warn about such a loadpoint (srcfl/ftw#1861).
func phaseLimits(cfg Config, mode string, site SiteFuse) Config {
	cfg.MinChargeW = phaseMinChargeW(cfg, mode, site)
	if cfg.MaxChargeW > 0 && cfg.MaxChargeW < cfg.MinChargeW {
		cfg.MaxChargeW = cfg.MinChargeW
	}
	inReach := func(w float64) bool {
		return w > 0 && w >= cfg.MinChargeW && (cfg.MaxChargeW <= 0 || w <= cfg.MaxChargeW)
	}
	if cfg.MinChargeW > 0 && len(cfg.AllowedStepsW) > 0 && !slices.ContainsFunc(cfg.AllowedStepsW, inReach) {
		cfg.AllowedStepsW = []float64{0, cfg.MinChargeW}
	}
	return cfg
}

// phaseMinChargeW is the smallest legal offer on the phases a command names:
// 6 A on one phase for 1p or auto, 6 A on each phase for 3p. An empty mode
// names no phases; the charger decides, and its configured minimum stands.
func phaseMinChargeW(cfg Config, mode string, site SiteFuse) float64 {
	if site.Phases() != 3 {
		return cfg.MinChargeW
	}
	voltage := site.Voltage
	if !finite(voltage) || voltage <= 0 {
		voltage = 230
	}
	switch mode {
	case "1p", "auto":
		return math.Max(cfg.MinChargeW/3, 6*voltage)
	case "3p":
		return math.Max(cfg.MinChargeW, 18*voltage)
	}
	return cfg.MinChargeW
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

// applyChargerPhaseLimit keeps a 1p order to a charger that can run three
// phases within the current that charger takes on one phase; otherwise a
// 25 A fuse lets 25 A onto one phase of a 16 A charger. It only lowers the
// order's limit. A 1p charger's maximum already caps its one phase, and on
// a one-phase site the maximum is the one-phase rating.
func applyChargerPhaseLimit(cmd map[string]any, cfg Config, site SiteFuse) {
	if mode, _ := cmd["phase_mode"].(string); mode != "1p" || cfg.PhaseMode == "1p" || site.Phases() != 3 {
		return
	}
	a := chargerPhaseAmps(cfg, site.Voltage)
	if a < 6 {
		return // Not a rating any charger has; leave the order as it is.
	}
	limit, _ := cmd["max_amps_per_phase"].(float64)
	if !finite(limit) || limit <= 0 || limit > a {
		cmd["max_amps_per_phase"] = a
	}
}

// chargerPhaseAmps is the most current the charger is known to take on one
// phase: its maximum spread over three phases, or the split below which
// auto puts it on one phase, whichever is larger. The maximum alone gives a
// one-phase charger left on an empty mode a third of its current (3,680 W
// is 5.3 A, so it never charges); the split alone gives a 32 A charger 16 A.
func chargerPhaseAmps(cfg Config, voltage float64) float64 {
	if !finite(voltage) || voltage <= 0 {
		voltage = 230
	}
	split := cfg.PhaseSplitW
	if !finite(split) || split <= 0 {
		split = defaultPhaseSplitW
	}
	maxW := cfg.MaxChargeW
	if !finite(maxW) || maxW < 0 {
		maxW = 0
	}
	return math.Max(maxW/(3*voltage), split/voltage)
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
