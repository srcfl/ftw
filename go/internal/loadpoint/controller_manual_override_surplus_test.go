package loadpoint

import (
	"context"
	"testing"
	"time"
)

// Manual override vs surplus_only. An explicit operator hold ("Start" /
// amp slider) outranks surplus_only: we honour the pinned charge rate
// even when there is no PV surplus (importing grid). surplus_only still
// governs automatic dispatch; releasing the hold drops back to it. The
// fuse clamp is the one guard a manual hold can never override.

// overrideLoadpoint is a 3-phase surplus_only loadpoint, mirroring
// Stefan's CTEK garage where the bug was found.
func overrideLoadpoint() Config {
	return Config{
		ID:            "garage",
		DriverName:    "ctek",
		MinChargeW:    4140, // 6 A @ 3Φ 230 V
		MaxChargeW:    11000,
		AllowedStepsW: ftwStepSet,
		PhaseMode:     "3p",
		SurplusOnly:   true,
	}
}

func TestManualHoldOverridesSurplusOnly(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cfg := overrideLoadpoint()
	dir := &Directive{
		SlotStart:         now.Add(-1 * time.Second),
		SlotEnd:           now.Add(60 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	// No PV surplus available. Pre-fix this clamped the hold to 0 W.
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })

	// Operator pins 6900 W (10 A @ 3Φ) via the amp slider — persistent.
	c.SetManualHold(cfg.ID, ManualHold{
		PowerW:     6900,
		PhaseMode:  "3p",
		Voltage:    230,
		Persistent: true,
	})
	c.Tick(context.Background(), now)

	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	if got := sender.calls[0].power; got != 6900 {
		t.Errorf("power = %.0f, want 6900 (manual hold overrides surplus_only, not clamped to 0)", got)
	}
}

func TestPersistentManualHoldNeverExpires(t *testing.T) {
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cfg := overrideLoadpoint()
	dir := &Directive{
		SlotStart:         base.Add(-1 * time.Second),
		SlotEnd:           base.Add(24 * time.Hour),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })

	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})

	// Far past any 30-minute diagnostic cap — the hold must still apply.
	later := base.Add(6 * time.Hour)
	if _, active := c.GetManualHold(cfg.ID, later); !active {
		t.Fatalf("persistent hold expired; want still active 6h later")
	}
	c.Tick(context.Background(), later)
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	if got := sender.calls[0].power; got != 4140 {
		t.Errorf("power = %.0f, want 4140 (persistent hold still applied 6h later)", got)
	}
}

func TestPersistentManualHoldClearedOnUnplug(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cfg := overrideLoadpoint()
	dir := &Directive{
		SlotStart:         now.Add(-1 * time.Second),
		SlotEnd:           now.Add(60 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	// Mutable sample — flip to disconnected for the second tick.
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })

	c.SetManualHold(cfg.ID, ManualHold{PowerW: 6900, PhaseMode: "3p", Persistent: true})
	if _, active := c.GetManualHold(cfg.ID, now); !active {
		t.Fatalf("hold not installed")
	}

	// Car unplugs.
	samples[cfg.DriverName] = EVSample{Connected: false}
	c.Tick(context.Background(), now.Add(5*time.Second))

	if _, active := c.GetManualHold(cfg.ID, now.Add(5*time.Second)); active {
		t.Errorf("persistent hold survived unplug; want cleared")
	}
}
