package loadpoint

import (
	"testing"
	"time"
)

// surplusTestConfig returns a loadpoint config typical for the
// surplus_only tests: 3Φ-eligible steps at 4140 W and 6900 W (Easee 6 A
// and 10 A on three phases at 230 V), 1Φ-eligible at 1380 W (6 A on one
// phase). The default phase split is 3680 W, so {1380} is 1Φ-only and
// {4140, 6900} are 3Φ-eligible.
func surplusTestConfig() Config {
	return Config{
		ID:            "lp1",
		MinChargeW:    1380,
		MaxChargeW:    11040,
		AllowedStepsW: []float64{0, 1380, 4140, 6900},
		PhaseSplitW:   3680,
		SurplusOnly:   true,
	}
}

// TestSurplusCmd_PauseResumeHysteresis verifies that the rolling-average
// pause/resume hysteresis matches the operator-stated intent: pause when
// avg drops below the 3Φ minimum, and don't resume until avg has climbed
// back to (3Φ-min + surplusResumeMarginW). Without the margin the
// loadpoint cycles the contactor every couple of ticks at the boundary.
func TestSurplusCmd_PauseResumeHysteresis(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := surplusTestConfig()

	// Drive surplusW via a closure variable so each tick can set its
	// own reading. The window holds 4 ticks, so we need 4 below-min
	// readings before the avg flips to "paused".
	var surplus float64
	c.SetSiteSurplusForEV(func() (float64, bool) { return surplus, true })

	// Anchor `now` well into the future so the pause-hold check always
	// passes by the time we ask for resume — surplusMinPauseHold is
	// 35 s, so we'll advance the clock by 60 s before checking resume.
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	// Four ticks below the 4140 W minimum drag the avg below it →
	// pause. Sample = 3000 W < 4140 W. Each tick first records the
	// sample (avg recomputes), then evaluates pause. The first three
	// ticks may still see a stale avg from initial buffer fill, so we
	// only assert on the fourth.
	surplus = 3000
	var got float64
	for i := 0; i < 4; i++ {
		got = c.computeSurplusCmd(now, cfg, 6900, 0)
	}
	if got != 0 {
		t.Fatalf("after 4 below-min ticks expected paused (got=%v)", got)
	}
	if !c.surplusPausedFor(cfg.ID) {
		t.Fatalf("expected paused state recorded for %s", cfg.ID)
	}

	// Surplus recovers to exactly the 3Φ-min: avg climbs but stays
	// strictly below (min + margin). After the pause-hold elapses we
	// must STILL be paused — the margin is the whole point.
	now = now.Add(60 * time.Second) // past surplusMinPauseHold (35s)
	surplus = 4140                  // exactly minStep3, no margin
	for i := 0; i < 4; i++ {
		got = c.computeSurplusCmd(now, cfg, 6900, 0)
		now = now.Add(5 * time.Second)
	}
	if got != 0 {
		t.Fatalf("at minStep3 with no margin expected still paused (got=%v)", got)
	}

	// Surplus crosses minStep3 + margin (4340 W): resume should fire
	// once avg passes the threshold. Same window of 4 samples to push
	// avg over the line.
	surplus = 5000
	resumed := false
	for i := 0; i < 4; i++ {
		got = c.computeSurplusCmd(now, cfg, 6900, 0)
		now = now.Add(5 * time.Second)
		if got > 0 {
			resumed = true
			break
		}
	}
	if !resumed {
		t.Fatalf("expected resume after avg ≥ minStep3+margin, last cmd=%v", got)
	}
}

// TestSurplusCmd_MinPauseHold verifies that even with the rolling avg
// instantly above the resume threshold, the loadpoint stays paused for
// at least surplusMinPauseHold after a pause edge. Without this guard
// Easee's contactor would flap on a 1-tick PV transient.
func TestSurplusCmd_MinPauseHold(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := surplusTestConfig()

	var surplus float64
	c.SetSiteSurplusForEV(func() (float64, bool) { return surplus, true })

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	// Force pause via 4 below-min ticks.
	surplus = 1000
	for i := 0; i < 4; i++ {
		c.computeSurplusCmd(now, cfg, 6900, 0)
	}
	if !c.surplusPausedFor(cfg.ID) {
		t.Fatalf("setup: expected paused")
	}

	// 10 s later (well under the 35 s hold) — surplus is huge, avg
	// will be massive, but the hold should keep us paused.
	now = now.Add(10 * time.Second)
	surplus = 9000
	for i := 0; i < 4; i++ {
		got := c.computeSurplusCmd(now, cfg, 6900, 0)
		if got != 0 {
			t.Fatalf("within %v of pause edge expected still paused, got=%v",
				surplusMinPauseHold, got)
		}
	}

	// 40 s after the pause edge — past the hold. Now resume should
	// fire on the first tick whose avg crosses the threshold.
	now = now.Add(35 * time.Second)
	resumed := false
	for i := 0; i < 4; i++ {
		got := c.computeSurplusCmd(now, cfg, 6900, 0)
		now = now.Add(5 * time.Second)
		if got > 0 {
			resumed = true
			break
		}
	}
	if !resumed {
		t.Fatalf("after surplusMinPauseHold and high surplus expected resume")
	}
}

// TestSurplusCmd_StepSnap verifies the magnitude side: the setpoint is
// the lower of (planner wantW, instant surplus) snapped to a 3Φ-eligible
// AllowedStepsW entry. Crucially, instant — not avg — drives magnitude;
// without that, a slow PV drop leaks into grid import (the user-stated
// rationale in computeSurplusCmd).
func TestSurplusCmd_StepSnap(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := surplusTestConfig()

	var surplus float64
	c.SetSiteSurplusForEV(func() (float64, bool) { return surplus, true })

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)

	// Plenty of surplus, planner wants 6900 W (the top step).
	// Output should snap to 6900 W exactly.
	surplus = 8000
	for i := 0; i < 4; i++ {
		// prime the rolling window so the pause hysteresis stays open
		c.computeSurplusCmd(now, cfg, 6900, 0)
	}
	if got := c.computeSurplusCmd(now, cfg, 6900, 0); got != 6900 {
		t.Fatalf("with 8000 W surplus + 6900 W wantW expected snap to 6900, got=%v", got)
	}

	// Surplus drops mid-charge to ~5500 W (between the 4140 and 6900
	// steps). Planner still wants 6900. We expect a snap DOWN to 4140
	// — the next step at-or-below `min(wantW, instant)`. Anything
	// higher would leak grid import.
	surplus = 5500
	for i := 0; i < 4; i++ {
		c.computeSurplusCmd(now, cfg, 6900, 0)
	}
	if got := c.computeSurplusCmd(now, cfg, 6900, 0); got != 4140 {
		t.Fatalf("with 5500 W surplus expected snap down to 4140, got=%v", got)
	}
}

// TestSurplusCmd_NoReader_ReturnsWantW verifies the test-path fallback:
// when no live surplus reader is wired, computeSurplusCmd returns wantW
// untouched. This is what unit tests of unrelated controller paths rely
// on so they don't have to mock a surplus source.
func TestSurplusCmd_NoReader_ReturnsWantW(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := surplusTestConfig()
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	if got := c.computeSurplusCmd(now, cfg, 4140, 0); got != 4140 {
		t.Fatalf("no-reader fallback should pass wantW through, got=%v", got)
	}
}

// TestSurplusCmd_StaleReader_Returns0 verifies the conservative fail-
// closed behaviour: if the live surplus reader returns ok=false (stale
// telemetry / no recent sample), we pause immediately rather than risk
// grid import on a guess.
func TestSurplusCmd_StaleReader_Returns0(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := surplusTestConfig()
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, false })
	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	if got := c.computeSurplusCmd(now, cfg, 4140, 0); got != 0 {
		t.Fatalf("stale reader expected pause-to-0, got=%v", got)
	}
}

// TestPickSurplusSteps_PhaseLockAndDayRollover verifies the 1Φ phase
// lock + the day-rollover unlock that uses the new tick-time argument.
// The lock is sticky: once the day's peak forecast can't sustain a 3Φ
// minimum we drop to the full 1Φ-eligible step set and stay there for
// the day. On the next local-day boundary, if forecast rebounds, we
// unlock. Without `now` plumbed through, the unlock timing wasn't
// testable — that's the point of C6.
func TestPickSurplusSteps_PhaseLockAndDayRollover(t *testing.T) {
	c := NewController(NewManager(), nil, nil, nil)
	cfg := surplusTestConfig()

	// Day 1 morning: peak forecast is bad (below the 4140 W 3Φ min).
	// pickSurplusSteps should lock 1Φ and return the full step set.
	var peak float64
	c.SetPeakRemainingSurplusW(func() (float64, bool) { return peak, true })
	day1 := time.Date(2026, 5, 3, 9, 0, 0, 0, time.Local)
	peak = 1500 // not enough for 3Φ
	steps := c.pickSurplusSteps(day1, cfg)
	if len(steps) != len(cfg.AllowedStepsW) {
		t.Fatalf("expected fall-back to all allowed steps when peak<minStep3, got=%v", steps)
	}
	if !c.surplusLockedTo1P(cfg.ID) {
		t.Fatalf("expected 1Φ lock after low-peak day")
	}

	// Same day, even if peak briefly recovers, the lock is sticky —
	// re-evaluating must not unlock mid-day (re-upgrade would flap
	// the contactor across the phase-mode boundary on every cloud).
	peak = 9000
	day1Afternoon := day1.Add(6 * time.Hour) // still day 1
	steps = c.pickSurplusSteps(day1Afternoon, cfg)
	if len(steps) != len(cfg.AllowedStepsW) {
		t.Fatalf("lock should be sticky within a day, got=%v", steps)
	}
	if !c.surplusLockedTo1P(cfg.ID) {
		t.Fatalf("expected 1Φ lock to survive same-day peak recovery")
	}

	// Next local day, peak forecast looks good: lock should clear and
	// pickSurplusSteps returns the 3Φ-only set again.
	day2 := day1.Add(24 * time.Hour)
	peak = 9000
	steps = c.pickSurplusSteps(day2, cfg)
	if len(steps) >= len(cfg.AllowedStepsW) {
		t.Fatalf("expected day-rollover unlock to return 3Φ subset, got=%v", steps)
	}
	if c.surplusLockedTo1P(cfg.ID) {
		t.Fatalf("expected 1Φ lock cleared on day-rollover with sufficient forecast")
	}

	// Day 3: bad peak again — re-locks. Confirms the lock isn't a
	// one-shot.
	day3 := day1.Add(48 * time.Hour)
	peak = 1000
	steps = c.pickSurplusSteps(day3, cfg)
	if len(steps) != len(cfg.AllowedStepsW) {
		t.Fatalf("expected re-lock on a second bad day, got=%v", steps)
	}
	if !c.surplusLockedTo1P(cfg.ID) {
		t.Fatalf("expected 1Φ re-lock on a fresh bad-forecast day")
	}
}

// surplusPausedFor is a test-only accessor for the per-loadpoint paused
// flag. Avoids exporting getSurplusPause from production code.
func (c *Controller) surplusPausedFor(id string) bool {
	paused, _ := c.getSurplusPause(id)
	return paused
}
