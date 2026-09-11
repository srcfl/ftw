package control

import (
	"math"
	"testing"
)

// A slew rate of zero is not a ramp limit — it is a stop.
//
// The limiter anchors on the battery's measured SmoothedW, so with
// SlewRateW = 0 every computed target is snapped back to whatever the
// battery is doing right now. The site then holds that power forever: no
// mode, plan or grid target can move it, and nothing is logged.
//
// It is reachable from the shipped UI. Settings → Control renders
// "Slew rate (W/cycle)" as a bare number input, there is no slew_enabled
// toggle next to it, and POST /api/config validates (slew_rate_w >= 0) but
// never runs applyDefaults — so an operator typing 0 to mean "no limit"
// gets a frozen battery until the process restarts.
//
// Non-positive therefore means "no external ramp limit", the same as
// slew_enabled: false.
func TestZeroSlewRateDoesNotFreezeDispatch(t *testing.T) {
	const gridW = 3000.0 // importing; self-consumption wants the battery to cover it

	store := seedStore(gridW, []struct {
		name          string
		currentW, soc float64
	}{
		{"ferroamp", 0, 0.5}, // battery idle — the anchor the limiter snaps to
	})
	st := NewState(0, 50, "ferroamp")
	st.Mode = ModeSelfConsumption
	st.SlewEnabled = true
	st.SlewRateW = 0

	targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want one battery target", targets)
	}
	if got := targets[0].TargetW; math.Abs(got) < 1 {
		t.Fatalf("target = %.0f W with slew_rate_w=0: dispatch is frozen at the measured anchor "+
			"while the site imports %.0f W", got, gridW)
	}
}

// The same site with the limiter off must reach the same command, which is
// what pins the intended meaning of a zero rate.
func TestZeroSlewRateMatchesSlewDisabled(t *testing.T) {
	const gridW = 3000.0


	run := func(enabled bool, rate float64) float64 {
		store := seedStore(gridW, []struct {
			name          string
			currentW, soc float64
		}{
			{"ferroamp", 0, 0.5},
		})
		st := NewState(0, 50, "ferroamp")
		st.Mode = ModeSelfConsumption
		st.SlewEnabled = enabled
		st.SlewRateW = rate
		targets := ComputeDispatch(store, st, caps(map[string]float64{"ferroamp": 15200}), 11040)
		if len(targets) != 1 {
			t.Fatalf("targets = %+v, want one battery target", targets)
		}
		return targets[0].TargetW
	}

	off := run(false, 3000)
	zero := run(true, 0)
	if math.Abs(off-zero) > 1 {
		t.Fatalf("slew_rate_w=0 commanded %.0f W, slew_enabled=false commanded %.0f W; "+
			"a zero rate must mean no external limit", zero, off)
	}
}
