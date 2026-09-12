package loadpoint

import (
	"context"
	"testing"
	"time"
)

// A 0 W offer (no plan, surplus pause, safety standdown) maps on Easee to a
// sticky user-pause at 0 A. The next non-zero offer must send ev_resume
// before ev_set_current, otherwise the contactor stays open until unplug.
func TestTickResumesChargerWhenOfferReturns(t *testing.T) {
	sender := &fakeSender{}
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4100, 7400, 11000},
	}}
	samples := map[string]EVSample{
		"easee": {PowerW: 0, Connected: true, RequestActive: true},
	}
	m := NewManager()
	m.Load(cfgs)
	var dir *Directive
	plan := PlanFunc(func(now time.Time) (Directive, bool) {
		if dir == nil {
			return Directive{}, false
		}
		return *dir, true
	})
	tel := TelemetryFunc(func(driver string) (EVSample, bool) {
		s, ok := samples[driver]
		return s, ok
	})
	c := NewController(m, plan, tel, sender.Send)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	c.Tick(context.Background(), now)
	if len(sender.calls) != 1 || sender.calls[0].action != "ev_set_current" || sender.calls[0].power != 0 {
		t.Fatalf("no-plan standdown: %+v", sender.calls)
	}
	if countAction(sender.calls, "ev_resume") != 0 {
		t.Fatalf("first 0 W command must not resume: %+v", sender.calls)
	}

	dir = &Directive{
		SlotStart:         now,
		SlotEnd:           now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{"garage": 2750},
	}
	c.Tick(context.Background(), now.Add(5*time.Second))
	if len(sender.calls) != 3 {
		t.Fatalf("reoffer command count = %d, want 3 (0 W, resume, set); got %+v", len(sender.calls), sender.calls)
	}
	if sender.calls[1].action != "ev_resume" {
		t.Fatalf("expected ev_resume after 0 W standdown, got %+v", sender.calls[1])
	}
	if sender.calls[2].action != "ev_set_current" || sender.calls[2].power != 11000 {
		t.Fatalf("expected 11000 W ev_set_current after resume, got %+v", sender.calls[2])
	}

	sender.calls = nil
	c.Tick(context.Background(), now.Add(10*time.Second))
	if countAction(sender.calls, "ev_resume") != 0 {
		t.Fatalf("must not resume again while already offering: %+v", sender.calls)
	}
	set, ok := lastSetCurrent(sender.calls)
	if !ok || set.power != 11000 {
		t.Fatalf("continued offer: %+v", sender.calls)
	}
}

func TestTickFirstPositiveOfferDoesNotResume(t *testing.T) {
	sender := &fakeSender{}
	slotStart := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	cfgs := []Config{{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4100, 7400, 11000},
	}}
	directive := &Directive{
		SlotStart:         slotStart,
		SlotEnd:           slotStart.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{"garage": 2750},
	}
	samples := map[string]EVSample{"easee": {PowerW: 0, Connected: true, RequestActive: true}}
	c := newTestController(t, cfgs, directive, samples, sender)

	c.Tick(context.Background(), slotStart)
	if len(sender.calls) != 1 || sender.calls[0].action != "ev_set_current" || sender.calls[0].power != 11000 {
		t.Fatalf("first offer of a process: %+v", sender.calls)
	}
	if countAction(sender.calls, "ev_resume") != 0 {
		t.Fatalf("first offer is not a standdown we issued: %+v", sender.calls)
	}
}
