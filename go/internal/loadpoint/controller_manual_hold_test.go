package loadpoint

import (
	"context"
	"testing"
	"time"
)

// Manual-hold tests. The hold is a diagnostics primitive: an operator
// pins a specific dispatch payload long enough to observe driver
// behaviour without fighting the 5-second control tick. Verifies the
// hold wins over MPC, expires correctly, and propagates every payload
// field — power, phase preferences, site fuse — verbatim.

func holdLoadpoint() Config {
	return Config{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1380,
		MaxChargeW:    11000,
		AllowedStepsW: ftwStepSet,
		PhaseMode:     "auto", // operator default; hold should override
		PhaseSplitW:   3680,
	}
}

// runHoldTick wires a controller, applies the hold, and returns the
// single sentCommand produced by Tick. now drives the hold's expiry
// comparison.
func runHoldTick(t *testing.T, cfg Config, hold ManualHold, now time.Time) sentCommand {
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
	c.SetManualHold(cfg.ID, hold)
	c.Tick(context.Background(), now)
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	return sender.calls[0]
}

func TestManualHoldOverridesPlannerBudget(t *testing.T) {
	now := time.Date(2026, 4, 26, 18, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint()
	cmd := runHoldTick(t, cfg, ManualHold{
		PowerW:    1380,
		PhaseMode: "1p",
		Voltage:   230,
		ExpiresAt: now.Add(60 * time.Second),
	}, now)
	if cmd.power != 1380 {
		t.Errorf("power = %.0f, want 1380 (hold value, not MPC's snap)", cmd.power)
	}
	if cmd.phaseMode != "1p" {
		t.Errorf("phase_mode = %q, want \"1p\" (hold override)", cmd.phaseMode)
	}
}

func TestManualHoldPropagatesSiteFuseFields(t *testing.T) {
	now := time.Date(2026, 4, 26, 18, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint()
	cmd := runHoldTick(t, cfg, ManualHold{
		PowerW:          4140,
		PhaseMode:       "auto",
		Voltage:         240, // non-standard mains
		MaxAmpsPerPhase: 20,
		SitePhases:      3,
		ExpiresAt:       now.Add(60 * time.Second),
	}, now)
	if cmd.voltage != 240 {
		t.Errorf("voltage = %.0f, want 240", cmd.voltage)
	}
	if cmd.maxAmpsPerPhase != 20 {
		t.Errorf("max_amps_per_phase = %.0f, want 20", cmd.maxAmpsPerPhase)
	}
	if cmd.sitePhases != 3 {
		t.Errorf("site_phases = %d, want 3", cmd.sitePhases)
	}
}

func TestManualHoldExpiresFallsBackToPlan(t *testing.T) {
	now := time.Date(2026, 4, 26, 18, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint()
	dir := &Directive{
		SlotStart:         now.Add(-1 * time.Second),
		SlotEnd:           now.Add(60 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	// Hold expired 1 second before this tick.
	c.SetManualHold(cfg.ID, ManualHold{
		PowerW:    1380,
		PhaseMode: "1p",
		ExpiresAt: now.Add(-1 * time.Second),
	})
	c.Tick(context.Background(), now)
	if len(sender.calls) != 1 {
		t.Fatalf("expected 1 send, got %d", len(sender.calls))
	}
	cmd := sender.calls[0]
	// Should have followed the plan, not the hold. Plan allocates
	// 6 kWh / 1 h = 6000 W, snapped to nearest step 6210 W (3Φ-flavored
	// budget). And phase_mode comes from lpCfg ("auto"), not hold ("1p").
	if cmd.power != 6210 {
		t.Errorf("power = %.0f, want 6210 (plan path), hold should have expired", cmd.power)
	}
	if cmd.phaseMode != "auto" {
		t.Errorf("phase_mode = %q, want \"auto\" (lpCfg path), hold should have expired", cmd.phaseMode)
	}
	// Expired hold should have been lazily evicted on the read.
	if _, active := c.GetManualHold(cfg.ID, now); active {
		t.Errorf("expired hold not evicted after read")
	}
}

func TestManualHoldClearImmediatelyRestoresPlan(t *testing.T) {
	now := time.Date(2026, 4, 26, 18, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint()
	dir := &Directive{
		SlotStart:         now.Add(-1 * time.Second),
		SlotEnd:           now.Add(60 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{
		PowerW:    1380,
		PhaseMode: "1p",
		ExpiresAt: now.Add(60 * time.Second),
	})
	c.ClearManualHold(cfg.ID)
	c.Tick(context.Background(), now)
	if sender.calls[0].power != 6210 {
		t.Errorf("power = %.0f after Clear, want 6210 (plan path)",
			sender.calls[0].power)
	}
}

func TestManualHoldZeroExpiresAtClearsHold(t *testing.T) {
	c := NewController(nil, nil, nil, nil)
	c.SetManualHold("garage", ManualHold{
		PowerW:    1380,
		ExpiresAt: time.Now().Add(time.Minute),
	})
	c.SetManualHold("garage", ManualHold{}) // zero ExpiresAt = clear
	if _, active := c.GetManualHold("garage", time.Now()); active {
		t.Errorf("hold not cleared when SetManualHold called with zero ExpiresAt")
	}
}

func TestManualHoldUnknownLoadpointIsNoOp(t *testing.T) {
	// Setting a hold for a loadpoint that isn't configured doesn't
	// crash and doesn't produce sends — Tick only iterates configured
	// loadpoints. This is just a safety check that the hold map can
	// hold orphan IDs without breakage.
	c := NewController(nil, nil, nil, nil)
	c.SetManualHold("ghost", ManualHold{
		PowerW:    1000,
		ExpiresAt: time.Now().Add(time.Minute),
	})
	// No Tick call — just verifying SetManualHold doesn't panic and
	// the orphan is retrievable.
	if _, active := c.GetManualHold("ghost", time.Now()); !active {
		t.Errorf("orphan hold lost")
	}
}

// Minimal hold (only power_w + expiry) MUST still propagate the
// loadpoint's PhaseMode and the controller's wired SiteFuse params
// (voltage, max_amps_per_phase, site_phases) into the cmd. Without
// the fall-through, the driver loses the per-phase fuse clamp inputs
// and falls back to its 230 V × 16 A defaults — wrong on
// non-standard sites. (Copilot review on PR #206.)
func TestManualHoldFallsThroughToLoadpointAndSiteDefaults(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint() // PhaseMode=auto, PhaseSplitW=3680
	cmd := runHoldTick(t, cfg, ManualHold{
		PowerW:    1380,
		ExpiresAt: now.Add(60 * time.Second),
	}, now)
	if cmd.power != 1380 {
		t.Errorf("power = %.0f, want 1380", cmd.power)
	}
	// Should fall through to lpCfg.PhaseMode + c.site.*
	if cmd.phaseMode != "auto" {
		t.Errorf("phase_mode = %q, want \"auto\" (fallthrough from lpCfg)", cmd.phaseMode)
	}
	if cmd.voltage != 230 {
		t.Errorf("voltage = %.0f, want 230 (fallthrough from site fuse)", cmd.voltage)
	}
	if cmd.maxAmpsPerPhase != 16 {
		t.Errorf("max_amps_per_phase = %.0f, want 16 (fallthrough from site fuse)",
			cmd.maxAmpsPerPhase)
	}
	if cmd.sitePhases != 3 {
		t.Errorf("site_phases = %d, want 3 (fallthrough from site fuse)", cmd.sitePhases)
	}
}

// Non-standard site (240 V × 20 A): even with a minimal hold, the
// driver receives THE SITE'S real fuse params, not the 230 V × 16 A
// driver default. This was the safety motivation for the
// fall-through.
func TestManualHoldFallsThroughToNonStandardSiteFuse(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint()
	dir := &Directive{
		SlotStart:         now.Add(-1 * time.Second),
		SlotEnd:           now.Add(60 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 20, Voltage: 240, PhaseCnt: 3})
	c.SetManualHold(cfg.ID, ManualHold{
		PowerW:    1500,
		ExpiresAt: now.Add(60 * time.Second),
	})
	c.Tick(context.Background(), now)
	cmd := sender.calls[0]
	if cmd.voltage != 240 {
		t.Errorf("voltage = %.0f, want 240 (real site mains)", cmd.voltage)
	}
	if cmd.maxAmpsPerPhase != 20 {
		t.Errorf("max_amps_per_phase = %.0f, want 20 (real site fuse)",
			cmd.maxAmpsPerPhase)
	}
}

// Explicit hold values still WIN over the loadpoint/site defaults —
// fall-through is for missing fields only. Operator-supplied
// overrides take precedence.
func TestManualHoldExplicitFieldsOverrideDefaults(t *testing.T) {
	now := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint() // PhaseMode=auto
	cmd := runHoldTick(t, cfg, ManualHold{
		PowerW:    1380,
		PhaseMode: "1p", // explicitly override the loadpoint's "auto"
		Voltage:   220,  // explicitly override the site's 230
		ExpiresAt: now.Add(60 * time.Second),
	}, now)
	if cmd.phaseMode != "1p" {
		t.Errorf("phase_mode = %q, want \"1p\" (operator override)", cmd.phaseMode)
	}
	if cmd.voltage != 220 {
		t.Errorf("voltage = %.0f, want 220 (operator override)", cmd.voltage)
	}
}
