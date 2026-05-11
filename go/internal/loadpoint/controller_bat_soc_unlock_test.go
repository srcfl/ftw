package loadpoint

import (
	"testing"
	"time"
)

// armCtrl wires a Controller with a bat-SoC reader and a surplus
// reader for the tests below. surplusW=>0 means "PV is producing
// enough to count as live surplus"; <=0 means "no PV".
func armCtrl(soc, surplusW *float64) *Controller {
	c := NewController(NewManager(), nil, nil, nil)
	c.SetBatSoCProvider(func() (float64, bool) {
		if soc == nil {
			return 0, false
		}
		return *soc, true
	})
	c.SetSiteSurplusForEV(func() (float64, bool) {
		if surplusW == nil {
			return 0, false
		}
		return *surplusW, true
	})
	return c
}

func TestEvalBatSoCArm_RequiresLivePV(t *testing.T) {
	soc := 0.85
	surplus := 1500.0 // PV producing
	c := armCtrl(&soc, &surplus)
	if !c.evalBatSoCArm("garage", 80) {
		t.Fatal("should arm: bat 85% >= threshold AND PV > 0")
	}
	// PV gone — staying armed for a few ticks (hysteresis), then released.
	surplus = -100
	for i := 0; i < batSoCPVGoneTicks-1; i++ {
		if !c.evalBatSoCArm("garage", 80) {
			t.Errorf("tick %d: should still be armed during PV-gone hysteresis", i)
		}
	}
	if c.evalBatSoCArm("garage", 80) {
		t.Error("after PV-gone hysteresis ticks expired, should release")
	}
}

func TestEvalBatSoCArm_PVReturnRearms(t *testing.T) {
	soc := 0.85
	surplus := 1500.0
	c := armCtrl(&soc, &surplus)
	c.evalBatSoCArm("garage", 80) // arm

	// Brief PV dip: 3 ticks no PV (below batSoCPVGoneTicks threshold)
	surplus = 0
	for i := 0; i < 3; i++ {
		c.evalBatSoCArm("garage", 80)
	}
	// PV comes back before hysteresis expires
	surplus = 1500
	if !c.evalBatSoCArm("garage", 80) {
		t.Error("brief PV dip must not disarm before hysteresis ticks expire")
	}
}

func TestEvalBatSoCArm_SoCBelowReleaseDisarms(t *testing.T) {
	soc := 0.85
	surplus := 1500.0
	c := armCtrl(&soc, &surplus)
	c.evalBatSoCArm("garage", 80) // arm
	soc = 0.74                    // below threshold-hyst (80-5=75) → release
	if c.evalBatSoCArm("garage", 80) {
		t.Error("soc 74% < 75% release floor must disarm regardless of PV")
	}
}

func TestEvalBatSoCArm_StalePreservesState(t *testing.T) {
	soc := 0.85
	surplus := 1500.0
	c := armCtrl(&soc, &surplus)
	c.evalBatSoCArm("garage", 80) // arm
	// Stale bat_soc — must preserve.
	c.SetBatSoCProvider(func() (float64, bool) { return 0, false })
	if !c.evalBatSoCArm("garage", 80) {
		t.Error("stale bat_soc reading must preserve previous arm state")
	}
}

func TestEvalBatSoCArm_ZeroThresholdDisables(t *testing.T) {
	soc := 0.99
	surplus := 5000.0
	c := armCtrl(&soc, &surplus)
	if c.evalBatSoCArm("garage", 0) {
		t.Error("threshold=0 must disable the unlock")
	}
}

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

// TestPickSurplusSteps_BatSoCArmedSkipsDailyLock asserts the new
// behaviour: when surplus dispatch is active *only* because of the
// bat-SoC unlock (not the configured SurplusOnly flag), the day-long
// 1Φ lock must NOT be set. That lock is an operator contract for
// configured surplus_only LPs; the opportunistic unlock is tick-level.
func TestPickSurplusSteps_BatSoCArmedSkipsDailyLock(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	// Force "peak surplus today is below the 3Φ min" so the original
	// path would lock to 1Φ.
	c.SetPeakRemainingSurplusW(func() (float64, bool) { return 100, true })
	cfg := Config{
		ID:            "garage",
		SurplusOnly:   false, // NOT configured surplus-only
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1380, 4140, 6900, 11000},
	}
	now := time.Date(2026, 5, 11, 20, 0, 0, 0, time.UTC)
	_ = c.pickSurplusSteps(now, cfg)
	if c.surplusLockedTo1P(cfg.ID) {
		t.Error("bat-SoC-armed surplus must NOT set the day-long 1Φ lock")
	}
}

func TestPickSurplusSteps_ConfiguredSurplusOnlyDoesLock(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	c.SetPeakRemainingSurplusW(func() (float64, bool) { return 100, true })
	cfg := Config{
		ID:            "garage",
		SurplusOnly:   true,
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1380, 4140, 6900, 11000},
	}
	now := time.Date(2026, 5, 11, 20, 0, 0, 0, time.UTC)
	_ = c.pickSurplusSteps(now, cfg)
	if !c.surplusLockedTo1P(cfg.ID) {
		t.Error("configured surplus_only with insufficient peak forecast must lock to 1Φ")
	}
}

// TestAnyLoadpointSurplusActive walks the combined-view aggregator
// main.go uses to decide whether to zero out battery PV-charge from
// the EV's apparent surplus (the flap protection).
func TestAnyLoadpointSurplusActive(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	soc := 0.85
	surplus := 1500.0
	c := NewController(m, nil, nil, nil)
	c.SetBatSoCProvider(func() (float64, bool) { return soc, true })
	c.SetSiteSurplusForEV(func() (float64, bool) { return surplus, true })

	// No schedule + SurplusOnly=false → false
	if c.AnyLoadpointSurplusActive() {
		t.Error("baseline: no surplus_only and no schedule → must be false")
	}

	// Schedule with bat-SoC unlock, but not yet evaluated (no Tick) → false
	m.SetSchedule("garage", Schedule{SurplusUnlockBatSoCPct: 80})
	if c.AnyLoadpointSurplusActive() {
		t.Error("schedule alone (without evalBatSoCArm being called) must not flip true")
	}

	// Evaluate arm — bat 85% with PV > 0 should arm; then aggregator true.
	c.evalBatSoCArm("garage", 80)
	if !c.AnyLoadpointSurplusActive() {
		t.Error("after arm via evalBatSoCArm, aggregator must report true")
	}

	// Disarm via SoC drop → aggregator back to false.
	soc = 0.50
	c.evalBatSoCArm("garage", 80)
	if c.AnyLoadpointSurplusActive() {
		t.Error("after disarm, aggregator must report false")
	}
}
