package loadpoint

import "testing"

func TestSurplusReserveW(t *testing.T) {
	tests := []struct {
		name   string
		states []State
		want   float64
	}{
		{
			name:   "empty",
			states: nil,
			want:   0,
		},
		{
			name: "ignores not-surplus and not-plugged",
			states: []State{
				{SurplusOnly: false, PluggedIn: true, CurrentPowerW: 0, MaxChargeW: 11000},
				{SurplusOnly: true, PluggedIn: false, CurrentPowerW: 0, MaxChargeW: 11000},
			},
			want: 0,
		},
		{
			name: "EV at 2.5kW with 11kW max → current + headroom",
			states: []State{
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 2500, MaxChargeW: 11000},
			},
			want: 2500 + EVRampHeadroomW,
		},
		{
			// EV plugged + surplus_only + not drawing, SoC UNKNOWN (dumb
			// charger like CTEK): reserve a bootstrap floor so the home
			// battery yields enough headroom for the EV to start. MinChargeW
			// is 0 here, so the floor falls back to EVRampHeadroomW. Without
			// this the EV could never bootstrap surplus charging (the battery
			// eats all PV → no surplus to claim → EV stays at 0 W).
			name: "EV at 0W, SoC unknown → bootstrap reserve (dumb charger)",
			states: []State{
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 0, MaxChargeW: 11000},
			},
			want: EVRampHeadroomW,
		},
		{
			name: "EV close to max → clamped to MaxChargeW (no overshoot)",
			states: []State{
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 10000, MaxChargeW: 11000},
			},
			want: 11000,
		},
		{
			name: "EV at max → reserve equals max",
			states: []State{
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 11000, MaxChargeW: 11000},
			},
			want: 11000,
		},
		{
			name: "multiple LPs sum — drawing LP + bootstrap for not-drawing",
			states: []State{
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 1500, MaxChargeW: 3700}, // 1500 + 2000 = 3500, under cap
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 0, MaxChargeW: 11000},   // not drawing, SoC unknown → bootstrap EVRampHeadroomW
			},
			want: 1500 + EVRampHeadroomW + EVRampHeadroomW,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SurplusReserveW(tt.states, nil)
			if got != tt.want {
				t.Errorf("SurplusReserveW = %.0f, want %.0f", got, tt.want)
			}
		})
	}
}

// Concrete regression: user's reported bug. EV at 2.5 kW on an Easee
// with 11 kW max, plan says charge battery, 3 kW of PV exporting.
// Pre-fix the reserve was 11 kW so ceiling = pvSurplus − (reserve −
// current) = 3000 − (11000 − 2500) = −5500 → 0; battery idled and
// the 3 kW crossed the meter at low spot price. Post-fix the reserve
// is `current + EVRampHeadroomW`, so a meaningful share of the
// surprise surplus reaches the battery.
func TestSurplusReserveWReleasesUnusedMaxToBattery(t *testing.T) {
	states := []State{
		{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 2500, MaxChargeW: 11000},
	}
	reserve := SurplusReserveW(states, nil)
	current := states[0].CurrentPowerW
	pvSurplus := 3000.0
	ceiling := pvSurplus - (reserve - current)
	if ceiling <= 0 {
		t.Fatalf("ceiling = %.0f W — battery should get some of the 3 kW surplus, was 0 pre-fix", ceiling)
	}
}

// Wake-kick active: even though CurrentPowerW is still 0 W, the
// wallbox is actively offering current and the EV is expected to ramp
// within the next tick. The reserve must hold the floor so the home
// battery doesn't snatch the freed surplus during the brief gap.
func TestSurplusReserveWHonoursWakeKickFloor(t *testing.T) {
	states := []State{
		{ID: "lp1", SurplusOnly: true, PluggedIn: true, CurrentPowerW: 0, MaxChargeW: 11000, MinChargeW: 1380},
	}
	wakeKick := map[string]bool{"lp1": true}
	reserve := SurplusReserveW(states, wakeKick)
	if reserve < 1380 {
		t.Errorf("wake-kick active should reserve at least MinChargeW (1380 W), got %.0f", reserve)
	}
}

// Boundary at the 50 W threshold: < 50 → no reserve, ≥ 50 → reserved.
// Locks the numeric contract so a future change to the threshold
// can't silently desync from callers reading it as documentation.
func TestSurplusReserveWThresholdBoundary(t *testing.T) {
	for _, tc := range []struct {
		power float64
		want  float64
	}{
		// Below 50 W = "not drawing"; SoC unknown (no vehicle fields) →
		// bootstrap floor (MinChargeW 0 → EVRampHeadroomW). At/above 50 W =
		// "drawing" → CurrentPowerW + EVRampHeadroomW.
		{0, EVRampHeadroomW},
		{49.9, EVRampHeadroomW},
		{50.0, 50.0 + EVRampHeadroomW}, // strict-less-than gate: exactly 50 W is on the "drawing" side
		{50.001, 50.001 + EVRampHeadroomW},
		{1380, 1380 + EVRampHeadroomW},
	} {
		states := []State{{SurplusOnly: true, PluggedIn: true, CurrentPowerW: tc.power, MaxChargeW: 11000}}
		got := SurplusReserveW(states, nil)
		if got != tc.want {
			t.Errorf("CurrentPowerW=%.3f: reserve=%.3f, want %.3f", tc.power, got, tc.want)
		}
	}
}

// 1Φ ladder climb: EV at 1Φ × 6 A (1380 W) should be able to step
// up several amps in one tick without the battery hoarding the
// surplus. The headroom needs to cover the largest practical
// single-tick climb that pickSurplusSteps will take on the 1Φ ladder
// — ~1Φ × 14 A (3220 W, +1840 W) sits inside 2 kW. After climbing
// the 1Φ ladder, the phase change (1Φ × 16 A → 3Φ × 6 A, +460 W) is
// trivial. The direct 1Φ × 6 A → 3Φ × 6 A jump (+2760 W) is NOT
// covered — that takes 2 ticks instead of 1, accepted on purpose to
// keep the headroom from re-imposing the user-reported bug on
// 3 kW-surplus scenarios.
func TestSurplusReserveWAllowsOnePhaseLadderClimb(t *testing.T) {
	states := []State{
		{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 1380, MaxChargeW: 11000},
	}
	reserve := SurplusReserveW(states, nil)
	reserveRemaining := reserve - states[0].CurrentPowerW
	const ladderClimbW = 3220 - 1380 // 1Φ × 6 A → 1Φ × 14 A ≈ 1840 W
	if reserveRemaining < ladderClimbW {
		t.Errorf("reserveRemaining = %.0f W — must be ≥ %.0f W so EV can climb 1Φ ladder in one tick",
			reserveRemaining, float64(ladderClimbW))
	}
}

// Plugged + Stopped + SoC below limit → reserve MinChargeW so the
// battery yields enough surplus for the next wake-kick to find.
// Regression for "Pixii grabs all PV, EV never starts" scenario.
func TestSurplusReserveWPluggedStoppedWithHeadroomReservesMin(t *testing.T) {
	states := []State{{
		ID: "garage", SurplusOnly: true, PluggedIn: true,
		CurrentPowerW:         0, // not drawing
		MinChargeW:            1380,
		MaxChargeW:            11000,
		VehicleSoCPct:         34, // below limit
		VehicleChargeLimitPct: 60,
	}}
	got := SurplusReserveW(states, nil)
	if got != 1380 {
		t.Errorf("SurplusReserveW = %.0f, want 1380 (MinChargeW) for stopped EV with SoC headroom", got)
	}
}

// Plugged + Stopped + SoC at limit → no reserve (Tesla taper-to-stop
// at 59/60: don't hold back battery for a vehicle that's effectively
// done charging).
func TestSurplusReserveWPluggedStoppedAtLimitNoReserve(t *testing.T) {
	states := []State{{
		ID: "garage", SurplusOnly: true, PluggedIn: true,
		CurrentPowerW:         0,
		MinChargeW:            1380,
		MaxChargeW:            11000,
		VehicleSoCPct:         60, // at limit
		VehicleChargeLimitPct: 60,
	}}
	if got := SurplusReserveW(states, nil); got != 0 {
		t.Errorf("SurplusReserveW = %.0f, want 0 (EV at limit, no headroom)", got)
	}
}

// Plugged + Stopped + SoC unknown (dumb charger like CTEK with no BMS
// readout) → reserve MinChargeW as a bootstrap. Without it, an EV on a
// charger that never reports SoC could never start surplus charging: the
// home battery absorbs all PV, no surplus is left to claim, the EV stays
// at 0 W and the reserve stays 0 W (chicken-and-egg). EV PV-charging is
// prioritised over the home battery. We skip ONLY a car KNOWN to be full
// (see TestSurplusReserveWPluggedStoppedAtLimitNoReserve).
func TestSurplusReserveWPluggedStoppedSoCUnknownBootstraps(t *testing.T) {
	states := []State{{
		ID: "garage", SurplusOnly: true, PluggedIn: true,
		CurrentPowerW: 0,
		MinChargeW:    1380,
		MaxChargeW:    11000,
		// VehicleSoCPct + VehicleChargeLimitPct both 0 (unknown — dumb charger)
	}}
	if got := SurplusReserveW(states, nil); got != 1380 {
		t.Errorf("SurplusReserveW = %.0f, want 1380 (MinChargeW bootstrap for dumb charger)", got)
	}
}
