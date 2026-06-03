package loadpoint

import (
	"context"
	"testing"
	"time"
)

// Manual charge-current is the user-facing "set my amps" override (the Tesla-app
// slider). Unlike the diagnostics manual_hold it persists until the user picks
// Auto or the car unplugs, overrides surplus/schedule (the user asked to charge
// now), but still respects the fuse. The controller converts amps→watts at the
// loadpoint's phase count and commands that explicit phase_mode so the driver's
// per-phase current equals the slider.

func runManualCurrentTick(t *testing.T, cfg Config, amps float64, now time.Time) (sentCommand, *Controller) {
	t.Helper()
	dir := &Directive{
		SlotStart:         now.Add(-1 * time.Second),
		SlotEnd:           now.Add(60 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetManualCurrent(cfg.ID, amps)
	c.Tick(context.Background(), now)
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	return sender.calls[0], c
}

func TestManualCurrentCommandsAmpsAndOverridesPlanner(t *testing.T) {
	now := time.Date(2026, 4, 26, 18, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint() // MaxChargeW 11000, PhaseMode "auto"
	cmd, _ := runManualCurrentTick(t, cfg, 10, now)
	// auto + 3-phase fuse → 3p; 10 A × 230 V × 3 = 6900 W (≤ 11000 cap).
	if cmd.power != 6900 {
		t.Errorf("power = %.0f, want 6900 (10 A × 230 V × 3φ)", cmd.power)
	}
	if cmd.phaseMode != "3p" {
		t.Errorf("phase_mode = %q, want \"3p\" so the driver sets 10 A/phase", cmd.phaseMode)
	}
}

func TestManualCurrentClampedToChargerMax(t *testing.T) {
	now := time.Date(2026, 4, 26, 18, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint()
	// 20 A × 230 × 3 = 13800 W, above the 11000 W charger ceiling → clamp.
	cmd, _ := runManualCurrentTick(t, cfg, 20, now)
	if cmd.power != cfg.MaxChargeW {
		t.Errorf("power = %.0f, want %.0f (clamped to charger max)", cmd.power, cfg.MaxChargeW)
	}
}

func TestManualCurrentClearedOnUnplug(t *testing.T) {
	now := time.Date(2026, 4, 26, 18, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint()
	dir := &Directive{SlotStart: now.Add(-time.Second), SlotEnd: now.Add(time.Hour),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000}}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetManualCurrent(cfg.ID, 12)
	if a, ok := c.GetManualCurrent(cfg.ID); !ok || a != 12 {
		t.Fatalf("precondition: manual current should be 12 A, got %v %v", a, ok)
	}
	// Car unplugs.
	samples[cfg.DriverName] = EVSample{Connected: false}
	c.Tick(context.Background(), now.Add(5*time.Second))
	if _, ok := c.GetManualCurrent(cfg.ID); ok {
		t.Error("manual current must be cleared on unplug")
	}
}
