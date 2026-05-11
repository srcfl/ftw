package loadpoint

import "testing"

// TestEvalBatSoCArm_Hysteresis covers the arm-at / release-below
// behaviour the surplus-unlock relies on.
func TestEvalBatSoCArm_Hysteresis(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	var soc float64
	c.SetBatSoCProvider(func() (float64, bool) { return soc, true })

	cases := []struct {
		name      string
		soc       float64
		threshold float64
		wantArmed bool
	}{
		{"below arm threshold starts disarmed", 0.70, 80, false},
		{"reaches threshold arms", 0.80, 80, true},
		{"slight dip stays armed (deadband)", 0.78, 80, true},
		{"drops to release boundary stays armed", 0.75, 80, true}, // 80-5 = 75, inclusive armed
		{"drops below release disarms", 0.749, 80, false},
		{"climbs back into deadband stays disarmed", 0.78, 80, false},
		{"climbs back above threshold re-arms", 0.80, 80, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			soc = tc.soc
			got := c.evalBatSoCArm("garage", tc.threshold)
			if got != tc.wantArmed {
				t.Errorf("soc=%v threshold=%v: got armed=%v want %v",
					tc.soc, tc.threshold, got, tc.wantArmed)
			}
		})
	}
}

// TestEvalBatSoCArm_ZeroThresholdDisables verifies the off switch.
func TestEvalBatSoCArm_ZeroThresholdDisables(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	c.SetBatSoCProvider(func() (float64, bool) { return 0.99, true })
	if c.evalBatSoCArm("garage", 0) {
		t.Error("threshold=0 must disable the unlock")
	}
}

// TestEvalBatSoCArm_StalePreservesState — a missing/stale reading
// must NOT change the arm state. Otherwise a one-tick blip during
// peak surplus would release the unlock.
func TestEvalBatSoCArm_StalePreservesState(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	stale := false
	c.SetBatSoCProvider(func() (float64, bool) {
		if stale {
			return 0, false
		}
		return 0.85, true
	})
	if !c.evalBatSoCArm("garage", 80) {
		t.Fatal("should arm on first call with soc=85")
	}
	stale = true
	if !c.evalBatSoCArm("garage", 80) {
		t.Error("stale reading must preserve armed state")
	}
}

// TestSurplusActive_NilProviderGracefullyOff makes sure a controller
// without a bat-soc provider behaves exactly as today: only the
// configured SurplusOnly flag matters.
func TestSurplusActive_NilProviderGracefullyOff(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := Config{ID: "garage", SurplusOnly: false}
	sched := Schedule{SurplusUnlockBatSoCPct: 80}
	if c.surplusActive(cfg, sched) {
		t.Error("nil bat-soc provider: surplusActive should be false when SurplusOnly is off")
	}
	cfg.SurplusOnly = true
	if !c.surplusActive(cfg, sched) {
		t.Error("SurplusOnly=true must always be surplusActive=true")
	}
}
