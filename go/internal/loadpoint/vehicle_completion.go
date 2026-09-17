package loadpoint

import "time"

// VehicleChargeState must come from a fresh, matched vehicle reading. A
// charger counter or an old BMS anchor cannot prove that charging is complete.
type VehicleChargeState struct {
	SoC   float64
	Limit float64
	State string
}

// VehicleObservationApplies rejects a reading from before this connection,
// including the first connection seen after restart. With several connected
// loadpoints, completion needs a car-to-charger binding that we do not have.
func (m *Manager) VehicleObservationApplies(id string, observedAt time.Time) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	lp := m.byID[id]
	if lp == nil || !lp.pluggedIn || observedAt.IsZero() || observedAt.Before(lp.connectionObservedAt) {
		return false
	}
	for otherID, other := range m.byID {
		if otherID != id && other.pluggedIn {
			return false
		}
	}
	return true
}

func (c *Controller) SetVehicleChargeState(read func(string) (VehicleChargeState, bool)) {
	if c != nil {
		c.vehicleChargeState = read
	}
}

// PlanningTarget uses the actual car limit when available. A vehicle-limit
// goal with no such reading reserves up to 100%; that is a planning bound,
// not a claim about the car's charge limit or current battery level.
func PlanningTarget(st State, vehicleLimit float64) float64 {
	target := st.TargetSoC
	if st.FinishAtVehicleLimit {
		target = 1
	}
	if vehicleLimit > 0 && vehicleLimit <= 1 && vehicleLimit < target {
		target = vehicleLimit
	}
	return target
}

// Keep safe current available after the model runs out of estimated energy
// or a deadline passes. The car decides when it is done. Ordinary price
// scheduling runs until then; manual Stop and all safety clamps still win.
func (c *Controller) vehicleCompletionOffer(cfg Config, now time.Time) (float64, bool) {
	st, ok := c.manager.State(cfg.ID)
	if !ok || !st.FinishAtVehicleLimit || !st.PluggedIn {
		return 0, false
	}
	if st.GoalComplete && !st.Schedule.Recurring {
		return 0, true
	}
	soc, target := st.CurrentSoC, PlanningTarget(st, 0)
	if c.vehicleChargeState != nil {
		if car, fresh := c.vehicleChargeState(cfg.ID); fresh {
			if car.State == "Complete" {
				if st.CurrentPowerW < DeliveringW {
					c.manager.completeVehicleGoal(cfg.ID)
					return 0, true
				}
				// Measured delivery contradicts Complete. Do not retain it.
				return cfg.MaxChargeW, true
			}
			if st.ChargingDeclined && (car.State == "Charging" || car.State == "Starting") {
				c.manager.RetryCharging(cfg.ID)
				st.ChargingDeclined = false
			}
			if st.GoalComplete && (car.State == "Charging" || car.State == "Starting" || (car.Limit > 0 && car.SoC < car.Limit)) {
				c.manager.resumeRecurringVehicleGoal(cfg.ID)
				c.manager.RetryCharging(cfg.ID)
				st.GoalComplete = false
				st.ChargingDeclined = false
			}
			soc, target = car.SoC, PlanningTarget(st, car.Limit)
		}
	}
	if st.GoalComplete || st.ChargingDeclined {
		return 0, true
	}
	if soc < target && (st.TargetTime.IsZero() || st.TargetTime.After(now)) {
		return 0, false
	}
	return cfg.MaxChargeW, true
}
