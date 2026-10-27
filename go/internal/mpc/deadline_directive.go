package mpc

import (
	"maps"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// withDeadlineReserve supplies current EV intent. It never activates or
// invents an optimizer plan, battery energy, a price or a solar forecast.
func (s *Service) withDeadlineReserve(now time.Time, d SlotDirective, accepted bool) (SlotDirective, bool) {
	if s.LoadpointStates == nil {
		return d, accepted
	}
	states := s.LoadpointStates() // No control lock: the caller may hold it.
	limits := s.PlanningLimits
	if limits == nil {
		limits = func(st loadpoint.State) (float64, float64, []float64) {
			return loadpoint.LimitPower(st, st.AllowedStepsW)
		}
	}
	if !accepted {
		d = SlotDirective{SlotStart: now, SlotEnd: now.Add(time.Minute), EVOnly: true}
	}
	for _, st := range states {
		if _, hasBudget := d.LoadpointEnergyWh[st.ID]; hasBudget && !st.ManualActive {
			continue // An accepted zero budget is a price decision, too.
		}
		minW, maxW, steps := limits(st)
		need, peak, ok := loadpoint.DeadlineNeed(st, minW, maxW, steps)
		if st.ManualActive && st.PluggedIn {
			// Manual intent has already passed the charger's own clamps.
			// Reserve its ceiling, including an explicit Stop, without
			// turning it into a saved scheduled goal. Full power below the
			// least order its phases run is that order (srcfl/ftw#1861).
			peak, ok = 0, true
			if finite(st.ManualChargeW) && st.ManualChargeW > 0 && finite(st.MaxChargeW) && st.MaxChargeW > 0 {
				peak = min(st.ManualChargeW, st.MaxChargeW)
				if peak == st.MaxChargeW && finite(minW) {
					peak = max(peak, minW)
				}
			}
			need = peak * d.SlotEnd.Sub(now).Hours()
		}
		if !ok || st.ID == "" || (st.SurplusOnly && !st.ManualActive) {
			continue
		}
		if !st.ManualActive {
			// Near the goal use the smallest legal offer that can deliver
			// this minute's need, rather than reserving the charger's full
			// rating for a final few Wh. A charger without a table takes
			// any power in its range.
			if len(st.AllowedStepsW) == 0 {
				steps = nil
			}
			peak = loadpoint.SnapChargeW(need/d.SlotEnd.Sub(now).Hours(), minW, peak, steps)
		}
		if d.LoadpointFallback == nil {
			d.LoadpointFallback = map[string]bool{}
			d.LoadpointEnergyWh = maps.Clone(d.LoadpointEnergyWh)
			d.LoadpointMaxPowerW = maps.Clone(d.LoadpointMaxPowerW)
			if d.LoadpointEnergyWh == nil {
				d.LoadpointEnergyWh = map[string]float64{}
			}
			if d.LoadpointMaxPowerW == nil {
				d.LoadpointMaxPowerW = map[string]float64{}
			}
		}
		// The state already includes delivery before now. Reserve Wh are
		// remaining goal energy, not a second budget from the slot's origin.
		d.LoadpointEnergyWh[st.ID] = min(need, peak*d.SlotEnd.Sub(now).Hours())
		d.LoadpointMaxPowerW[st.ID] = peak
		d.LoadpointFallback[st.ID] = true
	}
	return d, accepted || len(d.LoadpointFallback) > 0
}
