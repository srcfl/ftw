package loadpoint

import (
	"errors"
	"testing"
	"time"
)

func TestScheduleCheckedReportsStorageFailureWithoutChangingGoal(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	old := Schedule{SoC: .8, TimeOfDayMinUTC: 420, Recurring: true}
	m.SetSchedule("garage", old)
	m.RollSchedules(time.Now())
	before, _ := m.State("garage")
	failure := errors.New("disk full")
	m.SetScheduleSaver(func(string, Schedule) error { return failure })
	if ok, err := m.SetScheduleChecked("garage", Schedule{SoC: .9}); !ok || !errors.Is(err, failure) {
		t.Fatalf("set = %v, %v", ok, err)
	}
	if ok, err := m.ClearScheduleChecked("garage"); !ok || !errors.Is(err, failure) {
		t.Fatalf("clear = %v, %v", ok, err)
	}
	if m.SetSchedule("garage", Schedule{SoC: .9}) || m.ClearSchedule("garage") {
		t.Fatal("compatibility wrapper reported a failed save as successful")
	}
	if ok, err := m.SetScheduleChecked("missing", old); ok || err != nil {
		t.Fatalf("unknown ID = %v, %v", ok, err)
	}
	m.RollSchedules(time.Now())
	after, _ := m.State("garage")
	if after.Schedule != old || after.TargetSoC != before.TargetSoC || after.TargetTime != before.TargetTime {
		t.Fatalf("failed save changed the running goal: %+v", after)
	}
}

func TestScheduleWritesKeepOrderAndReadersSeeOldGoalDuringSave(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	m.SetSchedule("garage", Schedule{SoC: .8, TimeOfDayMinUTC: 420, Recurring: true})
	m.RollSchedules(time.Now())
	before, _ := m.State("garage")
	entered, release := make(chan struct{}), make(chan struct{})
	setDone, clearDone := make(chan error, 1), make(chan error, 1)
	var writes []Schedule
	m.SetScheduleSaver(func(_ string, s Schedule) error {
		if !s.Empty() {
			close(entered)
			<-release
		}
		writes = append(writes, s)
		return nil
	})
	go func() { _, err := m.SetScheduleChecked("garage", Schedule{SoC: .9}); setDone <- err }()
	<-entered
	go func() { _, err := m.ClearScheduleChecked("garage"); clearDone <- err }()
	select {
	case err := <-clearDone:
		t.Fatalf("Clear overtook an earlier pending save: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	readDone := make(chan State, 1)
	go func() { s, _ := m.State("garage"); readDone <- s }()
	select {
	case s := <-readDone:
		if s.Schedule != before.Schedule || s.TargetSoC != before.TargetSoC || s.TargetTime != before.TargetTime {
			t.Fatalf("new goal appeared before storage completed: %+v", s)
		}
	case <-time.After(time.Second):
		t.Fatal("disk write blocked state reads")
	}
	close(release)
	if err := <-setDone; err != nil {
		t.Fatal(err)
	}
	if err := <-clearDone; err != nil {
		t.Fatal(err)
	}
	if len(writes) != 2 || writes[0].SoC != .9 || !writes[1].Empty() {
		t.Fatalf("save order = %+v", writes)
	}
	if s, _ := m.State("garage"); !s.Schedule.Empty() || s.TargetSoC != 0 || !s.TargetTime.IsZero() {
		t.Fatalf("Clear lost to an older save: %+v", s)
	}
}
