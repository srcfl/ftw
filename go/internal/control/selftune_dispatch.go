package control

import (
	"math"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// SelfTuneDispatch builds the tick's commands while a self-tune run steps one
// battery: the tuned battery gets the step command and every other battery
// this dispatcher may command is held at 0 W. The fleet is holdFleetAtZero's,
// so a charger, meter, PV inverter or telemetry-only battery is never sent a
// battery command it can only refuse.
//
// The step is an operator request, not a licence to leave the envelope. The
// tuned command is first bounded like any fuse-relief target (power limits,
// the empty-pack and unknown-SoC discharge floor, reported direction blocks),
// then the set runs the same safety pipeline as idle and charge: fuse guard
// against live telemetry, the fuse-saver (which may still discharge a held
// sibling), the battery-boost reserve and the final limit clamp. Like a
// manual hold it skips the plan sign floor, which would otherwise zero a step
// that disagrees with the current plan slot.
func SelfTuneDispatch(
	store *telemetry.Store,
	state *State,
	driverCapacities map[string]float64,
	fuseMaxW float64,
	driver string,
	commandW float64,
) []DispatchTarget {
	targets := holdFleetAtZero(store, driverCapacities)
	for i := range targets {
		if targets[i].Driver != driver {
			continue
		}
		r := store.Get(driver, telemetry.DerBattery)
		if r == nil {
			break
		}
		var lim PowerLimits
		if state != nil {
			lim = state.DriverLimits[driver]
		}
		lower, upper := fuseTargetBounds(r, lim)
		targets[i].TargetW = math.Max(lower, math.Min(upper, commandW))
		targets[i].Clamped = targets[i].TargetW != commandW
		break
	}
	return applyDispatchSafetyPipeline(targets, store, state, driverCapacities, fuseMaxW, dispatchSafetyOptions{
		manualHoldActive:  true,
		updatePrevTargets: true,
		recordDispatch:    true,
	})
}
