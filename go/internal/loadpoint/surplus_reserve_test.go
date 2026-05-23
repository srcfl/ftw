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
			// Updated 2026-05: when EV is plugged but not actually
			// drawing, no reserve. The 2 kW idle headroom otherwise
			// hangs around for a Tesla that's Complete / a vehicle
			// driver that went offline / a car refusing the offer,
			// blocking the home battery from absorbing PV. Wake-kick
			// path (controller.tickOne) materialises EV draw to
			// >50 W within a tick if the EV is actually going to
			// start, at which point the reserve re-engages.
			name: "EV at 0W → no reserve (not drawing)",
			states: []State{
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 0, MaxChargeW: 11000},
			},
			want: 0,
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
			name: "multiple LPs sum — only active ones contribute",
			states: []State{
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 1500, MaxChargeW: 3700}, // 1500 + 2000 = 3500, under cap
				{SurplusOnly: true, PluggedIn: true, CurrentPowerW: 0, MaxChargeW: 11000},   // not drawing → 0
			},
			want: 1500 + EVRampHeadroomW,
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
		{0, 0},
		{49.9, 0},
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
