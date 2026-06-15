package flexload

import (
	"math"
	"testing"
)

// TestPlugLearnsRunningPower feeds a noisy on/off power trace and checks the
// learner recovers the appliance's running power and ignores the off state.
func TestPlugLearnsRunningPower(t *testing.T) {
	p := NewPlugProfile(0)
	now := int64(1_700_000_000_000)
	const step = 60_000 // 60s samples
	// Appliance runs at ~2000 W with ±100 W noise, duty-cycling off.
	for i := 0; i < 1000; i++ {
		w := 0.0
		if i%2 == 0 {
			w = 2000 + 100*math.Sin(float64(i))
		}
		p.Update(w, now, 300)
		now += step
	}
	if p.RunningW < 1900 || p.RunningW > 2100 {
		t.Errorf("learned RunningW = %.0f, want ~2000", p.RunningW)
	}
	// EffectivePowerW prefers a configured value, else learned.
	if got := p.EffectivePowerW(1500); got != 1500 {
		t.Errorf("configured power should win: got %.0f", got)
	}
	if got := p.EffectivePowerW(0); math.Abs(got-p.RunningW) > 1e-9 {
		t.Errorf("learned power should be used when unset: got %.0f", got)
	}
}

// TestPlugAccumulatesDailyEnergy verifies energy integration and day-rollover
// folding produce a sane daily-energy estimate.
func TestPlugAccumulatesDailyEnergy(t *testing.T) {
	p := NewPlugProfile(0)
	// Start at a UTC midnight so the first full day accumulates cleanly.
	const dayMs = 86_400_000
	now := int64(1_700_000_000_000)
	now = now - now%dayMs // align to UTC midnight
	const step = 60_000

	// Run a constant 1000 W for exactly 3 hours, then idle the rest of day 1.
	for t0 := int64(0); t0 < dayMs; t0 += step {
		w := 0.0
		if t0 < 3*3600_000 {
			w = 1000
		}
		p.Update(w, now, 300)
		now += step
	}
	// One sample into day 2 triggers the fold of day 1.
	p.Update(0, now, 300)

	// 1000 W × 3 h = 3000 Wh expected for day 1.
	if math.Abs(p.DailyEnergyWh-3000) > 60 { // ~2% tolerance for the step integration
		t.Errorf("DailyEnergyWh = %.0f, want ~3000", p.DailyEnergyWh)
	}
}

// TestPlugClassify checks the coarse device labels for representative shapes.
func TestPlugClassify(t *testing.T) {
	wh := &PlugProfile{Samples: 500, RunningW: 3000, DailyEnergyWh: 8000}
	if got := wh.Classify(); got != "water_heater" {
		t.Errorf("water heater classified as %q", got)
	}
	spa := &PlugProfile{Samples: 500, RunningW: 1500, DailyEnergyWh: 4000}
	if got := spa.Classify(); got != "spa_or_pool" {
		t.Errorf("spa classified as %q", got)
	}
	// Too few samples → unknown, regardless of shape.
	cold := &PlugProfile{Samples: 10, RunningW: 3000, DailyEnergyWh: 8000}
	if got := cold.Classify(); got != "unknown" {
		t.Errorf("under-trained profile should be unknown, got %q", got)
	}
}
