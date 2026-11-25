package main

import "github.com/srcfl/ftw/go/internal/loadpoint"

const (
	evStopHigh = 100.0 // W — "was actually drawing"
	evStopLow  = 50.0  // W — "now essentially zero"
)

// evStopNeedsReplan reports a plugged-in car that stopped drawing while Core
// still offered it power, so the plan's allocation is out of date. A stop
// Core ordered itself is already the plan's intent; replanning on it gave a
// fresh budget that restarted the charge and fed a start-stop loop.
func evStopNeedsReplan(lp loadpoint.State, prevW float64) bool {
	if !lp.PluggedIn || prevW < evStopHigh || lp.CurrentPowerW >= evStopLow {
		return false
	}
	return !lp.CommandedKnown || lp.CommandedW >= evStopLow
}

// declineReplanReason names a replan when a plugged-in car's refusal latch
// moved since the last tick (prevDeclined): the plan leaves the car out once
// it declined, and takes it in again once the latch ends (a retry, Charge
// now, a higher goal, measured charging). A scheduled replan would leave the
// old plan offering a declining car, or holding back a car that may charge,
// for up to its whole interval.
func declineReplanReason(lp loadpoint.State, prevDeclined bool) string {
	declined := lp.PluggedIn && lp.ChargingDeclined
	switch {
	case declined && !prevDeclined:
		return "loadpoint_declined"
	case prevDeclined && !declined && lp.PluggedIn:
		return "loadpoint_decline_ended"
	}
	return ""
}
