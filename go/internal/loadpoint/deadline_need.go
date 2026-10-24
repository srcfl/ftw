package loadpoint

import (
	"math"
)

// DeadlineNeed is the remaining AC energy and legal on-power of a saved goal,
// held to the session car's own charger and charge limit as the plan is. It
// does not promise future site capacity or authorize a hardware command.
func DeadlineNeed(st State) (energyWh, powerW float64, ok bool) {
	if !st.PluggedIn || st.ChargingDeclined || st.GoalComplete || st.TargetTime.IsZero() ||
		!finite(st.CurrentSoC) || st.CurrentSoC < 0 || st.CurrentSoC > 1 ||
		!finite(st.TargetSoC) || st.TargetSoC < 0 || st.TargetSoC > 1 ||
		!finite(st.VehicleCapacityWh) || st.VehicleCapacityWh <= 0 ||
		!finite(st.MinChargeW) || !finite(st.MaxChargeW) || st.MinChargeW <= 0 || st.MaxChargeW < st.MinChargeW {
		return 0, 0, false
	}
	// A vehicle-limit goal aims at 1, and the controller still stops at the
	// car's own completion; the car's own limit caps either.
	target := PlanningTarget(st, st.CarChargeLimit)
	energyWh = (target - st.CurrentSoC) * st.VehicleCapacityWh / DefaultChargeEfficiency
	if energyWh <= 0 || !finite(energyWh) {
		return 0, 0, false
	}
	for _, step := range st.AllowedStepsW {
		if !finite(step) || step < 0 || step > st.MaxChargeW {
			return 0, 0, false
		}
	}
	minW, maxW, steps := LimitPower(st, st.AllowedStepsW)
	for _, step := range steps {
		if step >= minW {
			powerW = math.Max(powerW, step)
		}
	}
	if len(st.AllowedStepsW) == 0 {
		powerW = maxW
	}
	return energyWh, powerW, powerW >= minW
}

// LimitPower holds a loadpoint's power to the session car's own charger: the
// lower of the charger's and the car's maximum, and the charger's steps up to
// it. When the charger's lowest step is above the car's maximum it keeps that
// one step, or without steps the charger's minimum: the car takes what it
// can, and a plan with no step would never charge. The minimum is never below
// the lowest step: PlanningSteps leaves out levels the charger's phases cannot
// run.
func LimitPower(st State, steps []float64) (minW, maxW float64, out []float64) {
	minW, maxW = st.MinChargeW, st.MaxChargeW
	lowest := 0.0
	for _, w := range steps {
		if w > 0 && (lowest == 0 || w < lowest) {
			lowest = w
		}
	}
	minW = math.Max(minW, lowest)
	if st.CarMaxW <= 0 || (maxW > 0 && st.CarMaxW >= maxW) {
		if maxW > 0 && minW > maxW {
			minW = maxW
		}
		return minW, maxW, steps
	}
	maxW = st.CarMaxW
	kept := false
	for _, w := range steps {
		if w <= maxW {
			out = append(out, w)
			kept = kept || w > 0
		}
	}
	if !kept && lowest > 0 {
		out = append(out, lowest)
		maxW = lowest
	}
	if lowest == 0 && minW > maxW {
		maxW = minW
	}
	if minW > maxW {
		minW = maxW
	}
	return minW, maxW, out
}

// CommandedEVPowerW uses the actual offers after every charger clamp. A
// charger need not have started drawing for its offer to occupy the fuse.
func CommandedEVPowerW(states []State) float64 {
	var total float64
	for _, w := range CommandedEVPowerByDriver(states) {
		total += math.Max(0, w)
	}
	return total
}

// CommandedEVPowerByDriver is each plugged charger's known offer, 0 W
// included, so a stopped car is still present. A driver with several
// loadpoints counts its highest offer.
func CommandedEVPowerByDriver(states []State) map[string]float64 {
	byDriver := make(map[string]float64, len(states))
	for _, st := range states {
		if !st.PluggedIn || !st.CommandedKnown || !finite(st.CommandedW) {
			continue
		}
		if w, seen := byDriver[st.DriverName]; !seen || st.CommandedW > w {
			byDriver[st.DriverName] = st.CommandedW
		}
	}
	return byDriver
}
