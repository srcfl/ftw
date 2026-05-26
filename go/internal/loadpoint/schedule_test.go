package loadpoint

import (
	"testing"
	"time"
)

func TestNextDailyUTC(t *testing.T) {
	base := time.Date(2026, 5, 10, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		now   time.Time
		min   int
		wantH int
		wantD int
	}{
		{"slot later today", base, 600, 10, 10}, // 10:00 today
		{"slot earlier today rolls to tomorrow", base, 360, 6, 11},
		{"slot at exact now rolls to tomorrow", base, 480, 8, 11},
		{"negative minUTC normalises", base, -60, 23, 10}, // 23:00 today (1440-60=1380 → 23:00)
		{"overflow minUTC normalises", base, 1500, 1, 11}, // 1500 mod 1440 = 60 = 01:00, < now → tomorrow
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NextDailyUTC(c.now, c.min)
			if got.Hour() != c.wantH || got.Day() != c.wantD {
				t.Errorf("NextDailyUTC(%v,%d) = %v, want H=%d D=%d",
					c.now, c.min, got, c.wantH, c.wantD)
			}
			if !got.After(c.now) {
				t.Errorf("NextDailyUTC must return time strictly after now; got %v <= %v",
					got, c.now)
			}
		})
	}
}

func TestScheduleEmpty(t *testing.T) {
	if !(Schedule{}).Empty() {
		t.Error("zero Schedule should be Empty")
	}
	if (Schedule{SoCPct: 50}).Empty() {
		t.Error("Schedule with SoCPct set should not be Empty")
	}
	if (Schedule{SurplusUnlockBatSoCPct: 80}).Empty() {
		t.Error("Schedule with bat-soc unlock should not be Empty")
	}
}

func TestManager_SetGetClearSchedule(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})

	if _, ok := m.GetSchedule("garage"); ok {
		t.Fatal("fresh manager should have no schedule")
	}
	if _, ok := m.GetSchedule("nope"); ok {
		t.Fatal("unknown ID should return ok=false")
	}

	s := Schedule{SoCPct: 50, TimeOfDayMinUTC: 360, Recurring: true, SurplusUnlockBatSoCPct: 80}
	if !m.SetSchedule("garage", s) {
		t.Fatal("SetSchedule should succeed for known ID")
	}
	got, ok := m.GetSchedule("garage")
	if !ok || got != s {
		t.Errorf("GetSchedule roundtrip mismatch: got %+v, want %+v", got, s)
	}
	if m.SetSchedule("nope", s) {
		t.Error("SetSchedule should reject unknown ID")
	}

	if !m.ClearSchedule("garage") {
		t.Fatal("ClearSchedule should succeed")
	}
	if _, ok := m.GetSchedule("garage"); ok {
		t.Error("ClearSchedule should remove it")
	}
}

func TestManager_RollSchedules_RecurringPromotes(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	s := Schedule{SoCPct: 50, TimeOfDayMinUTC: 360, Recurring: true}
	m.SetSchedule("garage", s)

	now := time.Date(2026, 5, 11, 7, 0, 0, 0, time.UTC) // past 06:00
	m.RollSchedules(now)

	st, _ := m.State("garage")
	if st.TargetSoCPct != 50 {
		t.Errorf("expected target_soc_pct=50 after roll, got %v", st.TargetSoCPct)
	}
	want := time.Date(2026, 5, 12, 6, 0, 0, 0, time.UTC)
	if !st.TargetTime.Equal(want) {
		t.Errorf("expected target_time=%v after roll, got %v", want, st.TargetTime)
	}

	// Idempotent: rolling again at the same time shouldn't push it again.
	m.RollSchedules(now)
	st, _ = m.State("garage")
	if !st.TargetTime.Equal(want) {
		t.Errorf("RollSchedules should be idempotent within the same window; got %v", st.TargetTime)
	}
}

func TestManager_RollSchedules_NonRecurringSeedsOnce(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	s := Schedule{SoCPct: 50, TimeOfDayMinUTC: 360, Recurring: false}
	m.SetSchedule("garage", s)

	// Now is 07:00; today's 06:00 has already passed → seed for tomorrow.
	now := time.Date(2026, 5, 11, 7, 0, 0, 0, time.UTC)
	m.RollSchedules(now)
	st, _ := m.State("garage")
	want := time.Date(2026, 5, 12, 6, 0, 0, 0, time.UTC)
	if !st.TargetTime.Equal(want) {
		t.Errorf("non-recurring first seed: target_time = %v, want %v",
			st.TargetTime, want)
	}
	if st.TargetSoCPct != 50 {
		t.Errorf("non-recurring first seed: target_soc_pct = %v, want 50", st.TargetSoCPct)
	}

	// After the deadline passes, RollSchedules must NOT re-seed —
	// non-recurring expires quietly. The schedule sticks around so
	// the operator can inspect it, but target_time stays in the past.
	after := want.Add(2 * time.Hour)
	m.RollSchedules(after)
	st, _ = m.State("garage")
	if !st.TargetTime.Equal(want) {
		t.Errorf("non-recurring after deadline: target_time should stay at %v, got %v",
			want, st.TargetTime)
	}
}

func TestManager_RollSchedules_RecurringSeedsFirstTarget(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	// No SetTarget called yet — schedule alone should populate it.
	s := Schedule{SoCPct: 50, TimeOfDayMinUTC: 360, Recurring: true}
	m.SetSchedule("garage", s)

	now := time.Date(2026, 5, 11, 7, 0, 0, 0, time.UTC)
	m.RollSchedules(now)

	st, _ := m.State("garage")
	if st.TargetSoCPct != 50 {
		t.Errorf("expected initial target_soc_pct=50 from schedule, got %v", st.TargetSoCPct)
	}
	want := time.Date(2026, 5, 12, 6, 0, 0, 0, time.UTC)
	if !st.TargetTime.Equal(want) {
		t.Errorf("expected initial target_time=%v, got %v", want, st.TargetTime)
	}
}

func TestManager_HydrateSchedules(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}, {ID: "street"}})
	want := Schedule{SoCPct: 80, TimeOfDayMinUTC: 420, Recurring: true, SurplusUnlockBatSoCPct: 75}
	m.HydrateSchedules(func(id string) (Schedule, bool) {
		if id == "garage" {
			return want, true
		}
		return Schedule{}, false
	})
	got, ok := m.GetSchedule("garage")
	if !ok || got != want {
		t.Errorf("HydrateSchedules: got=%+v ok=%v want=%+v", got, ok, want)
	}
	if _, ok := m.GetSchedule("street"); ok {
		t.Error("HydrateSchedules should not invent schedules for non-loaded IDs")
	}
}

// Regression: a stale one-shot target_time (set earlier via SetTarget or
// loaded from disk) used to silently shadow a freshly-saved schedule,
// because RollSchedules preserves future target_time values across
// ticks. SetSchedule must wipe the derived target so the next
// RollSchedules seeds the new deadline. Otherwise pressing Save on the
// schedule UI is a visible no-op — the MPC keeps planning against the
// old (often wrong) deadline.
func TestManager_SetScheduleOverridesStaleTargetTime_Recurring(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	now := time.Date(2026, 5, 12, 20, 0, 0, 0, time.UTC)
	// Operator (or stale state.db) leaves a one-shot target at 07:00
	// tomorrow morning.
	staleTarget := time.Date(2026, 5, 13, 5, 0, 0, 0, time.UTC)
	m.SetTarget("garage", 100, staleTarget)
	// Operator now saves a recurring schedule for 17:00 local CEST
	// (15:00 UTC = 900 min).
	m.SetSchedule("garage", Schedule{SoCPct: 100, TimeOfDayMinUTC: 900, Recurring: true})
	m.RollSchedules(now)
	st, _ := m.State("garage")
	want := time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC)
	if !st.TargetTime.Equal(want) {
		t.Errorf("recurring SetSchedule must override stale target_time; got %v, want %v",
			st.TargetTime, want)
	}
}

func TestManager_SetScheduleOverridesStaleTargetTime_NonRecurring(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	now := time.Date(2026, 5, 12, 20, 0, 0, 0, time.UTC)
	staleTarget := time.Date(2026, 5, 13, 5, 0, 0, 0, time.UTC)
	m.SetTarget("garage", 100, staleTarget)
	m.SetSchedule("garage", Schedule{SoCPct: 100, TimeOfDayMinUTC: 900, Recurring: false})
	m.RollSchedules(now)
	st, _ := m.State("garage")
	want := time.Date(2026, 5, 13, 15, 0, 0, 0, time.UTC)
	if !st.TargetTime.Equal(want) {
		t.Errorf("non-recurring SetSchedule must override stale target_time; got %v, want %v",
			st.TargetTime, want)
	}
}

func TestManager_LoadPreservesSchedule(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	s := Schedule{SoCPct: 50, TimeOfDayMinUTC: 360, Recurring: true}
	m.SetSchedule("garage", s)
	// Re-load (config hot reload): same id should keep its schedule.
	m.Load([]Config{{ID: "garage", DriverName: "easee", MaxChargeW: 11000}})
	got, ok := m.GetSchedule("garage")
	if !ok || got != s {
		t.Errorf("Load should preserve schedule across reload; got=%+v ok=%v", got, ok)
	}
}
