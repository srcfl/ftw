package loadpoint

import (
	"context"
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
	if !c.evalBatSoCArm("garage", 0.8) {
		t.Fatal("should arm: bat 85% >= threshold AND PV > 0")
	}
	// PV gone — staying armed for a few ticks (hysteresis), then released.
	surplus = -100
	for i := 0; i < batSoCPVGoneTicks-1; i++ {
		if !c.evalBatSoCArm("garage", 0.8) {
			t.Errorf("tick %d: should still be armed during PV-gone hysteresis", i)
		}
	}
	if c.evalBatSoCArm("garage", 0.8) {
		t.Error("after PV-gone hysteresis ticks expired, should release")
	}
}

func TestEvalBatSoCArm_PVReturnRearms(t *testing.T) {
	soc := 0.85
	surplus := 1500.0
	c := armCtrl(&soc, &surplus)
	c.evalBatSoCArm("garage", 0.8) // arm

	// Brief PV dip: 3 ticks no PV (below batSoCPVGoneTicks threshold)
	surplus = 0
	for i := 0; i < 3; i++ {
		c.evalBatSoCArm("garage", 0.8)
	}
	// PV comes back before hysteresis expires
	surplus = 1500
	if !c.evalBatSoCArm("garage", 0.8) {
		t.Error("brief PV dip must not disarm before hysteresis ticks expire")
	}
}

func TestEvalBatSoCArm_SoCBelowReleaseDisarms(t *testing.T) {
	soc := 0.85
	surplus := 1500.0
	c := armCtrl(&soc, &surplus)
	c.evalBatSoCArm("garage", 0.8) // arm
	soc = 0.74                     // below threshold-hyst (80-5=75) → release
	if c.evalBatSoCArm("garage", 0.8) {
		t.Error("soc 74% < 75% release floor must disarm regardless of PV")
	}
}

func TestEvalBatSoCArm_StalePreservesState(t *testing.T) {
	soc := 0.85
	surplus := 1500.0
	c := armCtrl(&soc, &surplus)
	c.evalBatSoCArm("garage", 0.8) // arm
	// Stale bat_soc — must preserve.
	c.SetBatSoCProvider(func() (float64, bool) { return 0, false })
	if !c.evalBatSoCArm("garage", 0.8) {
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
	sched := Schedule{SurplusUnlockBatSoC: 0.8}
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
	m.SetSchedule("garage", Schedule{SurplusUnlockBatSoC: 0.8})
	if c.AnyLoadpointSurplusActive() {
		t.Error("schedule alone (without evalBatSoCArm being called) must not flip true")
	}

	// Evaluate arm — bat 85% with PV > 0 should arm; then aggregator true.
	c.evalBatSoCArm("garage", 0.8)
	if !c.AnyLoadpointSurplusActive() {
		t.Error("after arm via evalBatSoCArm, aggregator must report true")
	}

	// Disarm via SoC drop → aggregator back to false.
	soc = 0.50
	c.evalBatSoCArm("garage", 0.8)
	if c.AnyLoadpointSurplusActive() {
		t.Error("after disarm, aggregator must report false")
	}
}

// TestSurplusAddsToPlan_OnlyWithTargetAndArm pins the split between the
// two surplus questions (#1060): surplusActive answers "does surplus
// REPLACE the plan", surplusAddsToPlan answers "may surplus be ADDED on
// top of it". With a schedule target and SurplusOnly off the arm adds;
// without a target it replaces (today's path); SurplusOnly always wins.
func TestSurplusAddsToPlan_OnlyWithTargetAndArm(t *testing.T) {
	soc := 0.85
	surplus := 1500.0
	c := armCtrl(&soc, &surplus)
	cfg := Config{ID: "garage"}
	withTarget := Schedule{SoC: 0.8, SurplusUnlockBatSoC: 0.8}
	noTarget := Schedule{SurplusUnlockBatSoC: 0.8}

	if !c.surplusAddsToPlan(cfg, withTarget) {
		t.Error("target + bat 85% + PV: surplus must add to the plan")
	}
	if c.surplusActive(cfg, withTarget) {
		t.Error("target set, surplus_only off: surplus must never replace the plan")
	}

	if c.surplusAddsToPlan(cfg, noTarget) {
		t.Error("no target: the arm replaces the plan, it does not add to it")
	}
	if !c.surplusActive(cfg, noTarget) {
		t.Error("no target + armed: surplus must replace the plan (unchanged path)")
	}

	cfg.SurplusOnly = true
	if c.surplusAddsToPlan(cfg, withTarget) {
		t.Error("surplus_only on: never additive")
	}
	if !c.surplusActive(cfg, withTarget) {
		t.Error("surplus_only on: always surplus-active, schedule or not")
	}
	cfg.SurplusOnly = false

	if c.surplusAddsToPlan(cfg, Schedule{SoC: 0.8}) {
		t.Error("no threshold: nothing to arm")
	}

	soc = 0.5 // below threshold − hysteresis → releases
	if c.surplusAddsToPlan(cfg, withTarget) {
		t.Error("home battery 50% < 75% release floor: plan only")
	}
}

// scheduledUnlockConfig is the shape the Scheduled tab produces: a target,
// "Also charge from PV surplus" and a "Home battery ≥ %" threshold saved
// together, with the PV-only flag off. Steps mirror an Easee at 230 V:
// 1380 W is 1Φ-only, {4140, 6900, 11000} are 3Φ-eligible under the
// default 3680 W phase split.
func scheduledUnlockConfig() Config {
	return Config{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1380, 4140, 6900, 11000},
	}
}

// scheduledUnlockTick wires a plugged-in scheduled loadpoint (target 80 %
// by 07:00, unlock at home battery ≥ 80 %) with a plan budget for the
// current 15-minute slot, a home-battery SoC and a live PV surplus, ticks
// once, and returns the command that reached the driver plus what the
// manager recorded as commanded.
func scheduledUnlockTick(t *testing.T, cfg Config, budgetWh, batSoC, surplusW float64) (sentCommand, float64, string) {
	t.Helper()
	base := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	sender := &fakeSender{}
	dir := &Directive{
		SlotStart:         base.Add(-1 * time.Second),
		SlotEnd:           base.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: budgetWh},
	}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.manager.SetSchedule(cfg.ID, Schedule{
		SoC: 0.8, TimeOfDayMinUTC: 7 * 60, Recurring: true, SurplusUnlockBatSoC: 0.8,
	})
	c.SetBatSoCProvider(func() (float64, bool) { return batSoC, true })
	c.SetSiteSurplusForEV(func() (float64, bool) { return surplusW, true })

	c.Tick(context.Background(), base)

	if len(sender.calls) != 1 {
		t.Fatalf("want exactly one command, got %d", len(sender.calls))
	}
	w, r := commandedReason(t, c, cfg.ID)
	return sender.calls[0], w, r
}

// TestTickScheduleUnlockAddsSurplusOverEmptyPlan is the bug in #1060: with
// a target set, the threshold control was inert. Home battery 85 % ≥ 80 %,
// 4.5 kW of spare PV, the plan has 0 W for this slot → the car gets the
// snapped surplus step (4140 W, nearest 3Φ-eligible step), not 0 W, and
// the reason says the watts came from surplus.
func TestTickScheduleUnlockAddsSurplusOverEmptyPlan(t *testing.T) {
	cfg := scheduledUnlockConfig()
	sent, w, r := scheduledUnlockTick(t, cfg, 0, 0.85, 4500)
	if sent.power != 4140 || w != 4140 || r != "pv_surplus" {
		t.Fatalf("want 4140 W / pv_surplus; sent %.0f W, recorded (%.0f W, %q)", sent.power, w, r)
	}
	// A scheduled charge keeps the schedule's phase behaviour: "auto" for
	// an unset operator mode, never the surplus 1Φ lock.
	if sent.phaseMode != "auto" {
		t.Errorf("phase_mode = %q, want auto while a schedule is active", sent.phaseMode)
	}
}

// TestTickScheduleUnlockNeverThrottlesPlan keeps the 2026-05-30 directive:
// the plan wants 11 kW of grid charge for the slot (2750 Wh over 15 min);
// 4.5 kW of surplus must not clamp it. The plan wins and keeps its reason.
func TestTickScheduleUnlockNeverThrottlesPlan(t *testing.T) {
	cfg := scheduledUnlockConfig()
	sent, w, r := scheduledUnlockTick(t, cfg, 2750, 0.85, 4500)
	if sent.power != 11000 || w != 11000 || r != "plan" {
		t.Fatalf("want 11000 W / plan; sent %.0f W, recorded (%.0f W, %q)", sent.power, w, r)
	}
}

// TestTickScheduleUnlockBelowThresholdIsPlanOnly: home battery at 50 % is
// below the 80 % threshold, so the arm stays off and the empty plan slot
// commands 0 W as before. Nothing may be attributed to surplus.
func TestTickScheduleUnlockBelowThresholdIsPlanOnly(t *testing.T) {
	cfg := scheduledUnlockConfig()
	sent, w, r := scheduledUnlockTick(t, cfg, 0, 0.5, 4500)
	if sent.power != 0 || w != 0 || r == "pv_surplus" {
		t.Fatalf("want 0 W from the plan; sent %.0f W, recorded (%.0f W, %q)", sent.power, w, r)
	}
}

// TestTickScheduleWithSurplusOnlyStillClampsToSurplus: with the PV-only
// flag on, a schedule changes nothing — surplus-only wins and the 11 kW
// plan is clamped to the live surplus step, exactly as before #1060.
func TestTickScheduleWithSurplusOnlyStillClampsToSurplus(t *testing.T) {
	cfg := scheduledUnlockConfig()
	cfg.SurplusOnly = true
	sent, w, r := scheduledUnlockTick(t, cfg, 2750, 0.85, 4500)
	if sent.power != 4140 || w != 4140 || r != "pv_surplus" {
		t.Fatalf("want 4140 W / pv_surplus; sent %.0f W, recorded (%.0f W, %q)", sent.power, w, r)
	}
}
