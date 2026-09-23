package loadpoint

import (
	"context"
	"testing"
	"time"
)

func TestRemoveScheduleClearsDerivedGoalButKeepsManualCharge(t *testing.T) {
	now := time.Date(2026, 9, 6, 20, 0, 0, 0, time.UTC)
	cfg := holdLoadpoint()
	m := NewManager()
	m.Load([]Config{cfg})
	m.SetSchedule(cfg.ID, Schedule{SoC: .8, TimeOfDayMinUTC: 420})
	m.RollSchedules(now)
	before, _ := m.State(cfg.ID)
	if before.TargetSoC != .8 || before.TargetTime.IsZero() {
		t.Fatal("test needs a rolled goal")
	}
	var saved Schedule
	m.SetScheduleSaver(func(_ string, s Schedule) error { saved = s; return nil })
	sender := &fakeSender{}
	c := NewController(m, func(time.Time) (Directive, bool) { return Directive{}, false }, func(string) (EVSample, bool) { return EVSample{Connected: true, RequestActive: true}, true }, sender.Send)
	c.SetManualHold(cfg.ID, ManualHold{PowerW: 4140, PhaseMode: "3p", Persistent: true})
	if !m.ClearSchedule(cfg.ID) {
		t.Fatal("goal removal failed")
	}
	m.RollSchedules(now.Add(time.Second))
	after, _ := m.State(cfg.ID)
	if !after.Schedule.Empty() || after.TargetSoC != 0 || !after.TargetTime.IsZero() || !saved.Empty() {
		t.Fatalf("removed goal still affects planning: %+v", after)
	}
	c.Tick(context.Background(), now.Add(time.Second))
	if _, ok := c.GetManualHold(cfg.ID, now); !ok {
		t.Fatal("removing a goal ended manual charging")
	}
	if len(sender.calls) != 1 || sender.calls[0].power != 4140 {
		t.Fatalf("manual charge changed: %+v", sender.calls)
	}
}
