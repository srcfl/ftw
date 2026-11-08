package mpc

import (
	"maps"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

// deadlineReserveMargin is one slot at the planner's longest (hourly) price
// slot.
const deadlineReserveMargin = time.Hour

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
	// Each saved goal's hours at its own peak. Cars may share one fuse
	// allowance, which the controller gives earliest deadline first, so a
	// goal's latest start counts every goal due at or before it (#1718).
	type goalHours struct {
		due   time.Time
		hours float64
	}
	var goals []goalHours
	for _, st := range states {
		if st.ID == "" || st.ManualActive || st.SurplusOnly {
			continue
		}
		minW, maxW, steps := limits(st)
		if need, peak, ok := loadpoint.DeadlineNeed(st, minW, maxW, steps); ok {
			goals = append(goals, goalHours{due: st.TargetTime, hours: need / peak})
		}
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
			// turning it into a saved scheduled goal.
			peak, ok = 0, true
			if finite(st.ManualChargeW) && st.ManualChargeW > 0 && finite(st.MaxChargeW) && st.MaxChargeW > 0 {
				peak = min(st.ManualChargeW, st.MaxChargeW)
			}
			need = peak * d.SlotEnd.Sub(now).Hours()
		}
		if !ok || st.ID == "" || (st.SurplusOnly && !st.ManualActive) {
			continue
		}
		if !st.ManualActive {
			// A goal with slack waits for its latest start: the time the
			// remaining need of every goal due by its deadline takes at
			// full power, one car after another, plus an hourly price slot.
			// Before that the car holds 0 W, as at startup, so a missing
			// plan does not start a car the next plan would pause (#1718).
			// A running reserve keeps a second slot, so one SoC step does
			// not stop and restart it.
			margin := deadlineReserveMargin
			if st.CommandedKnown && st.CommandedW > 0 {
				margin *= 2
			}
			var hours float64
			for _, g := range goals {
				if !g.due.After(st.TargetTime) {
					hours += g.hours
				}
			}
			latestStart := st.TargetTime.Add(-time.Duration(hours * float64(time.Hour))).Add(-margin)
			if now.Before(latestStart) {
				continue
			}
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
