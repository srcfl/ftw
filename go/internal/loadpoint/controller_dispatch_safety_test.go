package loadpoint

import (
	"context"
	"testing"
	"time"
)

// Integration regression: a scheduled charge keeps its persisted intent and
// observed charger state while a stale site meter closes the dispatch gate.
// The first fresh tick must resume the schedule rather than leaving a stale
// safety latch behind.
func TestStaleSiteMeterStopsScheduledChargeAndRecovers(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		ID:            "garage",
		DriverName:    "easee",
		MinChargeW:    1400,
		MaxChargeW:    11000,
		AllowedStepsW: []float64{0, 1400, 4100, 7400, 11000},
	}
	directive := &Directive{
		SlotStart:         now.Add(-time.Second),
		SlotEnd:           now.Add(15 * time.Minute),
		LoadpointEnergyWh: map[string]float64{cfg.ID: 2750},
	}
	sender := &fakeSender{}
	samples := map[string]EVSample{
		cfg.DriverName: {Connected: true, RequestActive: true},
	}
	c := newTestController(t, []Config{cfg}, directive, samples, sender)
	if !c.manager.SetSchedule(cfg.ID, Schedule{
		SoCPct:          80,
		TimeOfDayMinUTC: 13 * 60,
		Recurring:       true,
	}) {
		t.Fatal("failed to install schedule")
	}
	c.manager.RollSchedules(now)

	c.TickWithDispatch(context.Background(), now, false)
	if len(sender.calls) != 1 || sender.calls[0].power != 0 {
		t.Fatalf("stale site meter must stand scheduled EV down: %+v", sender.calls)
	}
	state, ok := c.manager.State(cfg.ID)
	if !ok || !state.PluggedIn || state.TargetSoCPct != 80 || state.TargetTime.IsZero() {
		t.Fatalf("schedule/observation did not stay live while blocked: %+v", state)
	}

	c.TickWithDispatch(context.Background(), now.Add(5*time.Second), true)
	if len(sender.calls) != 2 || sender.calls[1].power <= 0 {
		t.Fatalf("fresh site meter did not resume scheduled charge: %+v", sender.calls)
	}
}

// Persistent operator holds survive restarts and must also survive a safety
// pause. In particular, RequestActive=false while FTW is deliberately holding
// 0 W must not auto-release the hold; recovery resumes the pinned power.
func TestStaleSiteMeterStopsPersistentManualHoldAndRecovers(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	cfg := Config{ID: "garage", DriverName: "easee", MinChargeW: 1400, MaxChargeW: 11000}
	sender := &fakeSender{}
	samples := map[string]EVSample{
		cfg.DriverName: {Connected: true, RequestActive: false},
	}
	c := newTestController(t, []Config{cfg}, nil, samples, sender)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 6900, PhaseMode: "3p", Persistent: true})

	c.TickWithDispatch(context.Background(), now, false)
	c.TickWithDispatch(context.Background(), now.Add(SessionCompletionTimeout+time.Minute), false)
	if len(sender.calls) != 2 || sender.calls[0].power != 0 || sender.calls[1].power != 0 {
		t.Fatalf("stale site meter must keep persistent hold at 0 W: %+v", sender.calls)
	}
	if _, active := c.GetManualHold(cfg.ID, now.Add(SessionCompletionTimeout+time.Minute)); !active {
		t.Fatal("safety standdown incorrectly cleared persistent manual hold")
	}

	samples[cfg.DriverName] = EVSample{Connected: true, RequestActive: true}
	c.TickWithDispatch(context.Background(), now.Add(SessionCompletionTimeout+2*time.Minute), true)
	if len(sender.calls) != 3 || sender.calls[2].power != 6900 {
		t.Fatalf("fresh site meter did not resume persistent manual hold: %+v", sender.calls)
	}
}
