package loadpoint

import "testing"

func TestNextFusePhaseCapA(t *testing.T) {
	const fuse, margin, step = 16.0, 1.0, 1.0 // limit = 15
	cases := []struct {
		name              string
		prev, worst, want float64
	}{
		{"over-limit drops by overage", 16, 18, 13}, // 16-(18-15)
		{"deadband holds", 13, 15, 13},
		{"headroom ramps up", 13, 13, 14}, // 13+1=14<=15
		{"never exceeds fuse", 16, 10, 16},
		{"severe house overage floors at 0", 2, 20, 0},
		{"uninit starts at fuse", 0, 10, 16}, // 16, 10+1<=15 -> +1 -> 17 -> clamp 16
	}
	for _, tc := range cases {
		if got := nextFusePhaseCapA(tc.prev, tc.worst, fuse, margin, step); got != tc.want {
			t.Errorf("%s: nextFusePhaseCapA(prev=%v,worst=%v)=%v want %v", tc.name, tc.prev, tc.worst, got, tc.want)
		}
	}
}

// TestSiteFuseHotReloadAppliesToPerPhaseClamp guards the fix where the
// loadpoint's per-phase EV clamp ignored hot-reloaded fuse params (it read
// the startup-only c.site). After SetSiteFuse the clamp must use the new
// fuse: a worst phase that's safe at fuse=16 becomes a clamp at fuse=11.
func TestSiteFuseHotReloadAppliesToPerPhaseClamp(t *testing.T) {
	c := &Controller{}
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	if got := c.siteFuse().MaxAmps; got != 16 {
		t.Fatalf("siteFuse MaxAmps = %v, want 16", got)
	}
	c.SetPerPhaseMeterAmps(func() (float64, float64, float64, bool) { return 13, 5, 5, true })

	// fuse 16 (limit 15): worst 13 A is safe → no clamp.
	cmd := map[string]any{"max_amps_per_phase": 16.0}
	c.applyPerPhaseFuseClamp(Config{ID: "lp"}, cmd)
	if cmd["max_amps_per_phase"].(float64) != 16.0 {
		t.Fatalf("fuse 16, worst 13 A: expected no clamp, got %v", cmd["max_amps_per_phase"])
	}

	// Hot-reload the fuse down to 11 (limit 10): worst 13 A is now over.
	c.SetSiteFuse(SiteFuse{MaxAmps: 11, Voltage: 230, PhaseCnt: 3})
	if got := c.siteFuse().MaxAmps; got != 11 {
		t.Fatalf("after hot-reload siteFuse MaxAmps = %v, want 11", got)
	}
	// The servo ramps the cap down across ticks; within a couple it must
	// clamp the EV below the new fuse.
	var capped float64
	for i := 0; i < 3; i++ {
		cmd = map[string]any{"max_amps_per_phase": 11.0}
		c.applyPerPhaseFuseClamp(Config{ID: "lp"}, cmd)
		capped = cmd["max_amps_per_phase"].(float64)
	}
	if capped >= 11.0 {
		t.Fatalf("hot-reloaded fuse 11, worst 13 A: expected EV cap < 11, got %v", capped)
	}
}
