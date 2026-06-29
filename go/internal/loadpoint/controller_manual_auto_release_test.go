package loadpoint

import (
	"context"
	"testing"
	"time"
)

// When an operator pins a manual hold ("Start" / amp slider) and the
// vehicle then stops requesting current (it hit its own charge limit /
// is full), the hold should auto-release so the loadpoint drops back to
// automatic instead of pinning the wallbox at a fixed amperage forever —
// the operator shouldn't have to press Stop. Debounced by
// SessionCompletionTimeout so a brief ramp/handshake dip doesn't drop it.

func TestManualHoldAutoReleasesWhenVehicleFull(t *testing.T) {
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cfg := overrideLoadpoint()
	dir := &Directive{
		SlotStart:         base.Add(-1 * time.Second),
		SlotEnd:           base.Add(24 * time.Hour),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	// Car connected but NOT requesting current (full / declined).
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: false}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })

	c.SetManualHold(cfg.ID, ManualHold{PowerW: 6900, PhaseMode: "3p", Persistent: true})

	// First tick: the car is already not requesting, but the debounce
	// timer only just started — the hold must NOT release yet.
	c.Tick(context.Background(), base)
	if _, active := c.GetManualHold(cfg.ID, base); !active {
		t.Fatalf("hold released on the first not-requesting tick; want debounced")
	}

	// After the debounce window of sustained not-requesting, the hold
	// auto-releases — no operator Stop needed.
	later := base.Add(SessionCompletionTimeout + time.Second)
	c.Tick(context.Background(), later)
	if _, active := c.GetManualHold(cfg.ID, later); active {
		t.Errorf("manual hold not auto-released after %s of vehicle not requesting current", SessionCompletionTimeout)
	}
}

func TestManualHoldHeldWhileVehicleRequesting(t *testing.T) {
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cfg := overrideLoadpoint()
	dir := &Directive{
		SlotStart:         base.Add(-1 * time.Second),
		SlotEnd:           base.Add(24 * time.Hour),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	// Car actively drawing + requesting current.
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 6900, RequestActive: true}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })

	c.SetManualHold(cfg.ID, ManualHold{PowerW: 6900, PhaseMode: "3p", Persistent: true})

	c.Tick(context.Background(), base)
	later := base.Add(SessionCompletionTimeout + time.Second)
	c.Tick(context.Background(), later)
	if _, active := c.GetManualHold(cfg.ID, later); !active {
		t.Errorf("manual hold auto-released while the vehicle was actively requesting current")
	}
}

func TestManualHoldIdleTimerResetsOnResumedRequest(t *testing.T) {
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	cfg := overrideLoadpoint()
	dir := &Directive{
		SlotStart:         base.Add(-1 * time.Second),
		SlotEnd:           base.Add(24 * time.Hour),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 6000},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{cfg.DriverName: {Connected: true, PowerW: 0, RequestActive: false}}
	c := newTestController(t, []Config{cfg}, dir, samples, sender)
	c.SetSiteFuse(SiteFuse{MaxAmps: 16, Voltage: 230, PhaseCnt: 3})
	c.SetSiteSurplusForEV(func() (float64, bool) { return 0, true })

	c.SetManualHold(cfg.ID, ManualHold{PowerW: 6900, PhaseMode: "3p", Persistent: true})

	// Not requesting for a while (but under the window)…
	c.Tick(context.Background(), base)
	c.Tick(context.Background(), base.Add(60*time.Second))
	// …then the car resumes requesting — this must reset the idle timer.
	samples[cfg.DriverName] = EVSample{Connected: true, PowerW: 6900, RequestActive: true}
	c.Tick(context.Background(), base.Add(70*time.Second))
	// Car drops out again; only ~30 s of fresh idle by now < window.
	samples[cfg.DriverName] = EVSample{Connected: true, PowerW: 0, RequestActive: false}
	c.Tick(context.Background(), base.Add(100*time.Second))
	if _, active := c.GetManualHold(cfg.ID, base.Add(100*time.Second)); !active {
		t.Errorf("hold released too early — the idle timer should have reset when the car resumed requesting")
	}
}
