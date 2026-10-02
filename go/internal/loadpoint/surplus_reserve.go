package loadpoint

// EVRampHeadroomW is spare PV above the EV's live draw so the home
// battery does not take the next charger step. 2 kW covers a 1Φ climb
// and the 1Φ-max to 3Φ-min step. It does not cover a cold 1Φ×6 A to
// 3Φ×6 A jump; pickSurplusSteps walks the 1Φ ladder first, so that
// jump takes two ticks. 3 kW lined the reserve up with a site exporting
// 3 kW and left the battery nothing.
const EVRampHeadroomW = 2000

// GridChargeImportW is the live/plan grid band that means the site is
// deliberately importing, not soaking PV. Matches control's
// coverLoadChargeSlot / energy-path grid-charge skip.
const GridChargeImportW = 100.0

// SurplusReserveW is the PV headroom dispatch must leave for
// surplus-only loadpoints. wakeKickActiveIDs may be nil; those
// callers reserve only what is already drawing.
func SurplusReserveW(states []State, wakeKickActiveIDs map[string]bool) float64 {
	var sum float64
	for _, st := range states {
		// A force-charge is meant to pull from the battery. Reserving
		// surplus for it arms the no-discharge floor and flaps the battery
		// the moment grid hits zero (Stefan's CTEK, 2026-06-11).
		if !surplusOnlyProtected(st) {
			continue
		}
		// A wake-kick offers current before the car draws. Hold the
		// reserve or the battery takes the surplus and the kick aborts.
		if wakeKickActiveIDs[st.ID] {
			floor := st.MinChargeW
			if floor <= 0 {
				floor = EVRampHeadroomW
			}
			if st.CurrentPowerW > floor {
				floor = st.CurrentPowerW
			}
			ceiling := floor + EVRampHeadroomW
			if ceiling > st.MaxChargeW {
				ceiling = st.MaxChargeW
			}
			sum += ceiling
			continue
		}
		// Below 50 W is pilot or standby, not a claim on surplus.
		if st.CurrentPowerW < 50.0 {
			// A car that is not known full still needs a bootstrap floor,
			// including unknown SoC on a dumb wallbox. Otherwise the battery
			// takes all PV, the EV stays at 0 W, and the reserve stays 0 W.
			// A finished car on that wallbox holds the reserve until unplug.
			knownFull := st.VehicleSoC > 0 && st.VehicleChargeLimit > 0 &&
				st.VehicleSoC >= st.VehicleChargeLimit
			if knownFull {
				continue
			}
			floor := st.MinChargeW
			if floor <= 0 {
				floor = EVRampHeadroomW
			}
			if floor > st.MaxChargeW && st.MaxChargeW > 0 {
				floor = st.MaxChargeW
			}
			sum += floor
			continue
		}
		ceiling := st.CurrentPowerW + EVRampHeadroomW
		if ceiling > st.MaxChargeW {
			ceiling = st.MaxChargeW
		}
		if ceiling < 0 {
			ceiling = 0
		}
		sum += ceiling
	}
	return sum
}

// SurplusChargingW returns the actual positive charging power that belongs to
// loadpoints protected by SurplusReserveW. Dispatch keeps this separate from
// aggregate EV power so a regular loadpoint can still be covered by the home
// battery when another loadpoint is surplus-only.
func SurplusChargingW(states []State) float64 {
	var sum float64
	for _, st := range states {
		if !surplusOnlyProtected(st) || st.CurrentPowerW <= 0 {
			continue
		}
		sum += st.CurrentPowerW
	}
	return sum
}

func surplusOnlyProtected(st State) bool {
	return st.SurplusOnly && st.PluggedIn && !st.ManualActive
}

// SurplusPotentialW is the curtail reserve. SurplusReserveW is 0 for a
// plugged car that is not drawing, so the battery can take that PV.
// Curtail still leaves MaxChargeW when the car has SoC headroom, or
// unknown SoC. A known-full car is the only skip.
func SurplusPotentialW(states []State) float64 {
	var sum float64
	for _, st := range states {
		if !st.SurplusOnly || !st.PluggedIn {
			continue
		}
		if st.VehicleSoC > 0 && st.VehicleChargeLimit > 0 &&
			st.VehicleSoC >= st.VehicleChargeLimit {
			continue
		}
		head := st.MaxChargeW
		if head <= 0 {
			head = st.MinChargeW
		}
		if head <= 0 {
			head = EVRampHeadroomW
		}
		sum += head
	}
	return sum
}

// PlannerTreatsLoadpointAsSurplusOnly is the SurplusOnly flag the MPC spec
// should carry. The bat-SoC unlock is a this-tick opportunistic clamp;
// putting it on the 48 h spec forbids night-time grid EV in a plan that
// was computed while the sun was still up. Battery→EV is already blocked
// by NoBatteryToEV.
func PlannerTreatsLoadpointAsSurplusOnly(operatorSurplusOnly, deferGridPlan bool) bool {
	return operatorSurplusOnly || deferGridPlan
}

// SurplusAvailableForEVW is the PV leftover the surplus-only clamp may
// offer this tick. -gridW + batW + evW is house leftover. The EV tick
// runs before battery dispatch, so a battery still soaking PV is not
// yet free for the car. Import beyond the car is the battery buying.
func SurplusAvailableForEVW(gridW, batW, evW float64, surplusOnlyActive bool) float64 {
	leftover := -gridW + batW + evW
	if surplusOnlyActive {
		leftover -= PlannedPVSoakW(batW, gridW-evW)
	}
	if leftover < 0 {
		return 0
	}
	return leftover
}

// PlannedPVSoakW is the portion of a battery charge that is soaking
// leftover PV rather than buying from the grid. gridW is the grid
// flow attributed to house+battery (live meter minus EV, or planned
// GridW minus LoadpointW). A reading above the import band means the
// battery is buying, so soak is zero and leftover PV stays with the car.
func PlannedPVSoakW(batteryW, gridW float64) float64 {
	if batteryW <= 0 || gridW > GridChargeImportW {
		return 0
	}
	return batteryW
}
