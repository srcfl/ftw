package loadpoint

import "testing"

// The phase increase gate must read hot-reloaded fuse params, not the
// startup-only c.site: a worst phase with room at fuse=16 has none at 11.
func TestSiteFuseHotReloadAppliesToPhaseIncreaseGate(t *testing.T) {
	c := &Controller{}
	c.SetPerPhaseMeterAmps(func() (float64, float64, float64, bool) { return 13, 5, 5, true })
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	if got, known := c.sharedPhaseIncreaseW(nil); !known || got != 1380 {
		t.Fatalf("fuse 16, worst 13 A: increase=%v known=%v, want 1380 W", got, known)
	}
	c.SetSiteFuse(SiteFuse{MaxAmps: 11, Voltage: 230, PhaseCnt: 3})
	if got, known := c.sharedPhaseIncreaseW(nil); !known || got >= 0 {
		t.Fatalf("hot-reloaded fuse 11, worst 13 A: increase=%v known=%v, want none", got, known)
	}
}
