package control

import "testing"

// Slew anchors on measured SmoothedW. It must not walk a pack back into the
// discharge the distributor closed (SoC below the 5 % floor, or
// discharge_capable=false).
func TestSlewDoesNotReopenBlockedDischarge(t *testing.T) {
	cases := []struct {
		name       string
		soc        float64
		capability string
	}{
		{"soc_below_floor", 0.04, ""},
		{"discharge_capable_false", 0.50, `{"discharge_capable":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := NewState(0, 42, "meter")
			st.MinDispatchIntervalS = 0
			st.SlewRateW = 500
			store := seedDeadbandSite(2000, []deadbandBattery{{"bat", -3000, tc.soc, tc.capability}})
			out := ComputeDispatch(store, st, caps(map[string]float64{"bat": 10000}), 0)
			if got := targetsByDriver(out)["bat"].TargetW; got < -1 {
				t.Errorf("commanded %.0f W discharge to a pack that must not discharge", got)
			}
		})
	}
}

// When stopping the blocked pack at once would put the site over its fuse,
// keep the slewed ramp toward 0 W instead of adding the whole step as import.
// The ramp never adds discharge.
func TestBlockedDischargeKeepsRampWhenStopWouldTripFuse(t *testing.T) {
	st := NewState(0, 42, "meter")
	st.MinDispatchIntervalS = 0
	st.SlewRateW = 500
	store := seedDeadbandSite(10000, []deadbandBattery{{"bat", -3000, 0.04, ""}})
	out := ComputeDispatch(store, st, caps(map[string]float64{"bat": 10000}), 11040)
	got := targetsByDriver(out)["bat"].TargetW
	if got < -3000 || got > -1 {
		t.Errorf("target %.0f W, want a ramp between -3000 W and 0 W", got)
	}
}
