package loadpoint

import (
	"context"
	"testing"
	"time"
)

// A "charge now" hold (ReleaseAtSoC > 0) must release itself once the
// loadpoint's inferred SoC reaches the target, and the SAME tick must
// fall through to automatic plan dispatch — the whole point is that
// Start no longer kills the planner for the rest of the session.

func chargeNowLoadpoint() Config {
	return Config{
		ID:                "garage",
		DriverName:        "easee",
		MinChargeW:        4140,
		MaxChargeW:        11000,
		AllowedStepsW:     ftwStepSet,
		PhaseMode:         "3p",
		VehicleCapacityWh: 60000,
		PluginSoC:         0.5, // inferred SoC = 0.5 + session_wh/60000
	}
}

func TestChargeNowHoldReleasesAtTargetSoC(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	cfg := chargeNowLoadpoint()
	dir := &Directive{
		SlotStart:         base.Add(-1 * time.Second),
		SlotEnd:           base.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 0},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {
		Connected: true, PowerW: 11000, SessionWh: 6000, RequestActive: true,
	}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})

	c.SetManualHold(cfg.ID, ManualHold{
		PowerW: 11040, PhaseMode: "3p", Persistent: true, ReleaseAtSoC: 0.8,
	})

	// SoC = 0.5 + 6000/60000 = 0.6 — below target: the hold stays and
	// the hold wattage is what gets dispatched.
	c.Tick(context.Background(), base)
	if _, active := c.GetManualHold(cfg.ID, base); !active {
		t.Fatalf("hold released below its target SoC")
	}
	if n := len(sender.calls); n == 0 || sender.calls[n-1].power != 11000 {
		t.Fatalf("below target: want the 11000 W charger limit dispatched, got %+v", sender.calls)
	}

	// Session energy grows past the target: SoC = 0.5 + 18300/60000 =
	// 0.805 ≥ 0.8 — the hold releases and the SAME tick dispatches the
	// plan's allocation (0 Wh here → explicit 0 W standdown), not the
	// hold wattage.
	samples[cfg.DriverName] = EVSample{
		Connected: true, PowerW: 11000, SessionWh: 18300, RequestActive: true,
	}
	later := base.Add(30 * time.Second)
	c.Tick(context.Background(), later)
	if _, active := c.GetManualHold(cfg.ID, later); active {
		t.Errorf("hold still active after SoC reached its release target")
	}
	if n := len(sender.calls); n == 0 || sender.calls[n-1].power != 0 {
		t.Errorf("at target: want plan dispatch (0 W standdown), got %+v", sender.calls[len(sender.calls)-1])
	}
}

func TestLegacyHoldWithoutTargetNeverSoCReleases(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	cfg := chargeNowLoadpoint()
	sender := &fakeSender{}
	// Fully charged by the inference: SoC = 0.5 + 30000/60000 = 1.0.
	samples := map[string]EVSample{cfg.DriverName: {
		Connected: true, PowerW: 11000, SessionWh: 30000, RequestActive: true,
	}}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})

	c.SetManualHold(cfg.ID, ManualHold{PowerW: 11040, PhaseMode: "3p", Persistent: true})

	c.Tick(context.Background(), base)
	if _, active := c.GetManualHold(cfg.ID, base); !active {
		t.Errorf("legacy hold (no ReleaseAtSoC) must keep the pin-until-Stop-or-unplug contract")
	}
}
