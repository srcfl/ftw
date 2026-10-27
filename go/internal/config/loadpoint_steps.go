package config

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/srcfl/ftw/go/internal/units"
)

func (c *Config) normalizeLoadpointSteps() error {
	// Own the slice before repairing it: config reload keeps the old snapshot.
	loadpoints := slices.Clone(c.Loadpoints)
	for i := range loadpoints {
		lp := &loadpoints[i]
		if math.IsNaN(lp.MaxChargeW) || math.IsInf(lp.MaxChargeW, 0) || lp.MaxChargeW < 0 {
			return fmt.Errorf("charger %q: max charging power must be finite and non-negative", lp.ID)
		}
		if math.IsNaN(lp.MinChargeW) || math.IsInf(lp.MinChargeW, 0) || lp.MinChargeW < 0 || lp.MinChargeW > lp.MaxChargeW {
			return fmt.Errorf("charger %q: min charging power must be between 0 and max charging power", lp.ID)
		}
		for _, step := range lp.AllowedStepsW {
			if math.IsNaN(step) || math.IsInf(step, 0) || step < 0 {
				return fmt.Errorf("charger %q: charging steps must be finite and non-negative", lp.ID)
			}
		}
		steps, limited := units.LimitChargeSteps(lp.AllowedStepsW, lp.MaxChargeW)
		lp.AllowedStepsW = steps
		if limited {
			warning := fmt.Sprintf("Charger %q: charging steps above the %g W limit were reduced to %g W.", lp.ID, lp.MaxChargeW, lp.MaxChargeW)
			if !slices.Contains(c.LoadpointWarnings, warning) {
				c.LoadpointWarnings = append(c.LoadpointWarnings, warning)
			}
		}
	}
	c.Loadpoints = loadpoints
	c.warnMissingLoadpointConnections()
	c.warnLoadpointPhaseMinimum()
	return nil
}

const belowPhaseMinimum = "Below the phase minimum: "

// warnLoadpointPhaseMinimum names each charger whose maximum or step table
// stops below 6 A on each of its phases: three when pinned to 3p on a
// three-phase site, else one. Core orders that minimum instead
// (srcfl/ftw#1861). The warning goes when the loadpoint is fixed.
func (c *Config) warnLoadpointPhaseMinimum() {
	warnings := make([]string, 0, len(c.LoadpointWarnings))
	for _, warning := range c.LoadpointWarnings {
		if !strings.HasPrefix(warning, belowPhaseMinimum) {
			warnings = append(warnings, warning)
		}
	}
	voltage := c.Fuse.Voltage
	if math.IsNaN(voltage) || math.IsInf(voltage, 0) || voltage <= 0 {
		voltage = 230
	}
	for _, lp := range c.Loadpoints {
		phases, amps, fix := "one phase", "6 A", ""
		minimumW := 6 * voltage
		if lp.PhaseMode == "3p" && (c.Fuse.Phases <= 0 || c.Fuse.Phases == 3) {
			phases, amps, fix = "three phases", "6 A on each", ", or set the phase mode to auto if the charger can switch to one phase"
			minimumW *= 3
		}
		var problem, change string
		switch {
		case lp.MaxChargeW > 0 && lp.MaxChargeW < minimumW:
			problem = fmt.Sprintf("Its maximum of %g W is below that", lp.MaxChargeW)
			change = fmt.Sprintf("Raise the maximum to %g W or more", minimumW)
		case len(lp.AllowedStepsW) > 0 && !slices.ContainsFunc(lp.AllowedStepsW, func(w float64) bool {
			return w >= minimumW && (lp.MaxChargeW <= 0 || w <= lp.MaxChargeW)
		}):
			problem = fmt.Sprintf("Its charging steps have no level from %g W", minimumW)
			change = fmt.Sprintf("Add a step of %g W or more", minimumW)
		default:
			continue
		}
		warnings = append(warnings, belowPhaseMinimum+fmt.Sprintf(
			"charger %q on %s cannot charge below %g W (%s). %s, so FTW charges at %g W. %s%s.",
			lp.ID, phases, minimumW, amps, problem, minimumW, change, fix))
	}
	c.LoadpointWarnings = warnings
}
