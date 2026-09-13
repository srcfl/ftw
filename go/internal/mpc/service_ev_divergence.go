package mpc

import (
	"math"
	"time"
)

// A restored level or charging outside the plan changes the remaining duty.
// Reuse the existing reactive loop and cooldown, including when PV/load
// divergence checks are off. No new user setting is needed.
func (s *Service) loadpointStateDiverged(plan *Plan, params Params, now time.Time) bool {
	var current []*LoadpointSpec
	slotLen := 15
	if len(plan.Actions) > 0 && plan.Actions[0].SlotLenMin > 0 {
		slotLen = plan.Actions[0].SlotLenMin
	}
	if s.Loadpoints != nil {
		current = s.Loadpoints(slotLen)
	} else if s.Loadpoint != nil {
		if lp := s.Loadpoint(slotLen); lp != nil {
			current = []*LoadpointSpec{lp}
		}
	} else {
		return false
	}
	previous := params.activeLoadpoints()
	for _, lp := range current {
		if !lp.active() {
			continue
		}
		var initial *LoadpointSpec
		for _, old := range previous {
			if old.ID == lp.ID {
				initial = old
				break
			}
		}
		if initial == nil {
			return true
		}
		expected := initial.InitialSoC
		efficiency := initial.ChargeEfficiency
		if efficiency <= 0 {
			efficiency = .9
		}
		for _, a := range plan.Actions {
			start, end := a.ExecutionStart(), a.SlotStartMs+int64(a.SlotLenMin)*60000
			elapsed := min(now.UnixMilli(), end) - start
			if elapsed <= 0 {
				continue
			}
			watts := a.LoadpointPowerW[lp.ID]
			if len(a.LoadpointPowerW) == 0 && len(previous) == 1 {
				watts = a.LoadpointW
			}
			expected += max(0, watts) * float64(elapsed) / 3600000 * efficiency / initial.CapacityWh
		}
		// Two percentage points avoid replanning for rounding and normal meter
		// delay. A larger restoration/correction must not wait fifteen minutes.
		if math.Abs(lp.InitialSoC-min(1, expected)) > .02 {
			return true
		}
	}
	return false
}
