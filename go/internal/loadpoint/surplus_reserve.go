package loadpoint

// EVRampHeadroomW is the per-LP buffer added on top of the EV's
// current draw when computing the surplus reserve. It needs to cover
// one single-amp step on the EV's current phase mode so the
// controller can ramp up between ticks without the battery hoarding
// the surplus.
//
// 2000 W comfortably covers:
//   - any single-amp 1Φ step (+230 W) and 3Φ step (+690 W)
//   - climbing 1Φ × 6 A → 1Φ × 14 A in one tick (+1840 W)
//   - 1Φ-max → 3Φ-min crossover (1Φ × 16 A 3680 W → 3Φ × 6 A 4140 W,
//     +460 W) — the realistic phase-change step the EV takes after
//     climbing the 1Φ ladder
//
// What 2 kW does NOT cover is the direct 1Φ × 6 A → 3Φ × 6 A
// cold-start jump (+2760 W). In practice pickSurplusSteps walks the
// 1Φ ladder first, so this transition takes ~2 dispatch ticks (≈ 10 s)
// instead of 1. Acceptable trade vs. the alternative — sizing the
// headroom to that worst-case (3 kW) recreates the operator-reported
// bug where the user's "3 kW exporting" scenario lines up exactly
// with the reserve and the battery gets nothing.
//
// Tracking the LP's actual next reachable step via AllowedStepsW
// would tighten this further; deferred until 2 kW proves wrong on a
// real site.
const EVRampHeadroomW = 2000

// SurplusReserveW returns the aggregate PV headroom that must be
// preserved for surplus_only loadpoints. For each surplus_only +
// plugged_in LP it reserves min(MaxChargeW, CurrentPowerW +
// EVRampHeadroomW) so the reserve tracks the EV's actual draw rather
// than its theoretical max.
//
// Dispatch consumes the result via control.State.EVSurplusOnlyReserveW
// in both the energy and the legacy/reactive paths.
func SurplusReserveW(states []State) float64 {
	var sum float64
	for _, st := range states {
		if !st.SurplusOnly || !st.PluggedIn {
			continue
		}
		// Skip the reserve when the vehicle is in a terminal non-drawing
		// state. "Complete" means the car reached its SoC target and
		// won't draw more without operator intervention (re-target via
		// app, plug-cycle, force_start). Leaving 2 kW reserved for a car
		// that's refusing the offer makes the home battery hold steady
		// at the current SoC while the same 2 kW exports to grid — the
		// "charging a bit but not full surplus" symptom from a session
		// where the EV finished charging mid-afternoon. Other states
		// ("Stopped", "Disconnected", empty) keep the reserve so the
		// system can bootstrap a re-start.
		if st.VehicleChargingState == "Complete" {
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
