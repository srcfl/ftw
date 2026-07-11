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
// SurplusReserveW takes the loadpoint states plus the set of LP IDs
// whose wake-kick window is currently active. Wake-kick LPs get the
// reserve floor (CurrentPowerW probably still 0 W while the EV is
// ramping) so the home battery doesn't grab the freed surplus during
// the brief gap between the wallbox offering current and the EV
// actually starting to draw. Non-wake LPs that aren't drawing
// (CurrentPowerW < 50 W) contribute nothing — they're not actively
// claiming the surplus.
//
// wakeKickActiveIDs may be nil; callers without a wake-state source
// pass nil and the reserve degrades to the actual-draw rule only.
func SurplusReserveW(states []State, wakeKickActiveIDs map[string]bool) float64 {
	var sum float64
	for _, st := range states {
		if !st.SurplusOnly || !st.PluggedIn {
			continue
		}
		// Manual / schedule override: when the operator is force-charging
		// (manual hold, or an active schedule forcing the setpoint), the EV
		// is NOT surplus-gated — it charges at the forced power and the home
		// battery is EXPECTED to discharge to cover it ("battery covers EV"
		// is the intended mode here). Contribute no reserve for such an LP:
		// (a) reserving surplus export for it is pointless (it isn't waiting
		// on surplus), and (b) a non-zero reserve arms the dispatch
		// no-discharge floor, which cuts the battery the instant it covers
		// the EV (grid→0) and flaps the battery support 0↔full (observed on
		// Stefan's CTEK 2026-06-11 during a manual charge). surplus_only stays
		// on so automatic surplus charging resumes when the force-charge ends.
		if st.ManualActive {
			continue
		}
		// Tie the reserve to the EV's ACTUAL draw, not just "plugged in
		// + surplus_only". A car that's Complete, refusing the offer,
		// or whose vehicle driver has gone offline (Tesla proxy flake
		// etc.) reports CurrentPowerW≈0 — leaving 2 kW reserved for it
		// makes the home battery hold steady at SoC while the same
		// 2 kW exports to grid.
		//
		// Wake-kick override: during the kick window the wallbox is
		// actively offering current to a not-yet-drawing EV. Holding
		// the reserve at the LP's min charge level keeps the battery
		// from snatching the freed surplus before the EV's contactor
		// settles. Without this, a car slow to ramp (cold pack,
		// settling time) would see the surplus disappear into the
		// home battery within one tick and the wake-kick would abort.
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
		// Threshold 50 W picks up any non-trivial draw while ignoring
		// idle pilot / standby consumption that doesn't represent
		// "EV is actively claiming surplus".
		if st.CurrentPowerW < 50.0 {
			// Plugged-but-not-drawing fallback: when the vehicle's SoC
			// is KNOWN and below its charge limit, the car is merely
			// waiting for the wallbox to find surplus — not refusing
			// the offer. Reserve its MinChargeW so the home battery
			// yields enough headroom for the next wake-kick to have
			// something to hand the EV.
			//
			// Without this reservation the dispatch lets the battery
			// absorb every watt of PV surplus, then auto-wake fires,
			// the wallbox offers current — but no surplus is left to
			// claim because the battery already took it — so the EV
			// times back to Stopped, the auto-wake cycle retries, and
			// the EV never gets to charge.
			//
			// Reserve a bootstrap floor unless the car is KNOWN to be
			// full. Two cases want the reserve:
			//   1. Smart charger / paired vehicle: SoC is known and below
			//      its charge limit — the car is waiting for surplus.
			//   2. Dumb charger (CTEK and other AC wallboxes with no BMS
			//      readout): SoC is unknown entirely. Be optimistic and
			//      treat "plugged + surplus_only + not drawing" as "wants
			//      to charge" — otherwise an EV on a dumb charger can NEVER
			//      bootstrap surplus charging: the home battery absorbs
			//      every watt of PV, the loadpoint sees no surplus to
			//      claim, the EV stays at 0 W, and the reserve stays 0 W
			//      (chicken-and-egg). Prioritising PV into the EV ahead of
			//      the home battery is the intended behaviour.
			// We skip ONLY when SoC is known AND at/above the charge limit
			// (a genuinely full car). Trade-off for case 2: a finished-but-
			// still-plugged car on a dumb charger holds the reserve
			// (exporting instead of charging the home battery) until it's
			// unplugged — surfacing the charger's own "done" state into the
			// loadpoint State would let us skip that too (follow-up).
			knownFull := st.VehicleSoCPct > 0 && st.VehicleChargeLimitPct > 0 &&
				st.VehicleSoCPct >= st.VehicleChargeLimitPct
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

// SurplusPotentialW is the parallel reserve sized for the PV-curtail
// decision rather than for the dispatch's battery-vs-EV split.
//
// SurplusReserveW intentionally returns 0 for plugged-but-not-drawing
// EVs so the home battery can keep absorbing PV (a stopped EV refusing
// the offer must not block the battery from claiming surplus). That
// rule is right for dispatch but wrong for curtail: when curtail is
// economically warranted and the home battery is full, a stopped EV
// with SoC headroom *would* start drawing if PV were allowed to grow
// above its min charge — so PV should not be cut.
//
// Rule here: every surplus_only + plugged_in loadpoint whose vehicle
// still has SoC headroom (vehicle_soc < vehicle_charge_limit, or
// either is unknown — be optimistic when telemetry is partial)
// contributes its MaxChargeW. That's the upper bound on what curtail
// must preserve PV headroom for. Drivers report "no headroom" by
// setting VehicleSoCPct >= VehicleChargeLimitPct, which excludes
// already-full EVs from the calculation.
func SurplusPotentialW(states []State) float64 {
	var sum float64
	for _, st := range states {
		if !st.SurplusOnly || !st.PluggedIn {
			continue
		}
		// Skip when the vehicle is already at/above its charge limit
		// — both must be > 0 for the comparison to be meaningful.
		if st.VehicleSoCPct > 0 && st.VehicleChargeLimitPct > 0 &&
			st.VehicleSoCPct >= st.VehicleChargeLimitPct {
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
