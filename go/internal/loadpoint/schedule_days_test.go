package loadpoint

import (
	"encoding/json"
	"testing"
	"time"
)

// The weekday mask. Bit 0 = Monday, ISO order. Zero means every day —
// the value every schedule stored before the field existed decodes to
// — and the weekday is read in the box's own time zone, because a
// 00:30 Saturday deadline in Stockholm is still Friday in UTC and
// "weekdays" has to mean the household's weekdays.

const maskMonToFri = 0b0011111 // bits 0..4

func stockholm(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatalf("loading Europe/Stockholm: %v", err)
	}
	return loc
}

// A zero mask is exactly the pre-mask behaviour: NextDeadlineUTC and
// NextDailyUTC agree on every case the old function was tested on,
// and a stored schedule without the field decodes to that zero.
func TestNextDeadlineUTC_ZeroMaskIsEveryDay(t *testing.T) {
	loc := stockholm(t)
	base := time.Date(2026, 5, 10, 8, 0, 0, 0, time.UTC)
	for _, min := range []int{0, 360, 480, 600, 1439} {
		s := Schedule{SoC: 0.8, TimeOfDayMinUTC: min}
		if got, want := s.NextDeadlineUTC(base, loc), NextDailyUTC(base, min); !got.Equal(want) {
			t.Errorf("zero mask, min=%d: got %v, want NextDailyUTC's %v", min, got, want)
		}
	}

	// A schedule saved before the field existed carries no "days" key
	// and must decode to the every-day mask.
	var old Schedule
	if err := json.Unmarshal([]byte(`{"soc_pct":80,"time_of_day_min_utc":360,"recurring":true}`), &old); err != nil {
		t.Fatalf("decoding a pre-mask schedule: %v", err)
	}
	if old.Days != 0 {
		t.Fatalf("a pre-mask schedule decoded to days=%d, want 0 (every day)", old.Days)
	}
}

// Friday evening, weekdays-only: Saturday and Sunday are skipped and
// the deadline lands on Monday, at the same stored UTC time-of-day.
func TestNextDeadlineUTC_WeekdayMaskRollsPastTheWeekend(t *testing.T) {
	loc := stockholm(t)
	s := Schedule{SoC: 0.8, TimeOfDayMinUTC: 360, Recurring: true, Days: maskMonToFri}
	// Friday 2026-05-15 07:00 UTC — today's 06:00 slot has passed.
	now := time.Date(2026, 5, 15, 7, 0, 0, 0, time.UTC)
	got := s.NextDeadlineUTC(now, loc)
	want := time.Date(2026, 5, 18, 6, 0, 0, 0, time.UTC) // Monday
	if !got.Equal(want) {
		t.Fatalf("weekday mask: got %v (%v), want Monday %v", got, got.Weekday(), want)
	}
}

// The case the local-zone rule exists for: a 22:30 UTC deadline is
// 00:30 the NEXT day in Stockholm. Read in UTC the candidate is
// Friday and a weekdays-only mask would accept it; read in the
// household's zone it is Saturday and must be skipped. The mask keeps
// skipping until Sunday 22:30 UTC — Monday 00:30 local.
func TestNextDeadlineUTC_WeekdayIsTheHouseholdsNotUTCs(t *testing.T) {
	loc := stockholm(t)
	s := Schedule{SoC: 0.8, TimeOfDayMinUTC: 22*60 + 30, Days: maskMonToFri}
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC) // Friday noon UTC
	got := s.NextDeadlineUTC(now, loc)

	utcFriday := time.Date(2026, 5, 15, 22, 30, 0, 0, time.UTC)
	if got.Equal(utcFriday) {
		t.Fatal("the mask was read in UTC: Friday 22:30 UTC is already Saturday in Stockholm")
	}
	want := time.Date(2026, 5, 17, 22, 30, 0, 0, time.UTC) // Monday 00:30 local
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v (Monday 00:30 in Stockholm)", got, want)
	}
}

// A single-day mask reaches its day from anywhere in the week.
func TestNextDeadlineUTC_SingleDayMask(t *testing.T) {
	loc := stockholm(t)
	s := Schedule{SoC: 0.8, TimeOfDayMinUTC: 360, Days: 1 << 6} // Sunday only
	now := time.Date(2026, 5, 18, 7, 0, 0, 0, time.UTC)         // Monday
	got := s.NextDeadlineUTC(now, loc)
	want := time.Date(2026, 5, 24, 6, 0, 0, 0, time.UTC) // next Sunday
	if !got.Equal(want) {
		t.Fatalf("Sunday-only mask from Monday: got %v (%v), want %v", got, got.Weekday(), want)
	}
}

// The manager path: RollSchedules promotes a Friday-evening recurring
// schedule straight to Monday when the mask says weekdays, in the
// zone the manager was pinned to.
func TestManager_RollSchedules_WeekdayMaskSkipsWeekend(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	m.SetLocation(stockholm(t))
	m.SetSchedule("garage", Schedule{
		SoC: 0.8, TimeOfDayMinUTC: 360, Recurring: true, Days: maskMonToFri,
	})

	// Friday 2026-05-15 19:00 UTC, past today's 06:00 slot.
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	m.RollSchedules(now)

	st, _ := m.State("garage")
	want := time.Date(2026, 5, 18, 6, 0, 0, 0, time.UTC) // Monday
	if !st.TargetTime.Equal(want) {
		t.Fatalf("target_time = %v (%v), want Monday %v", st.TargetTime, st.TargetTime.Weekday(), want)
	}
	if st.TargetSoC != 0.8 {
		t.Fatalf("target_soc_pct = %v, want 80", st.TargetSoC)
	}
}

// The same roll with a zero mask still lands on Saturday — old stored
// schedules and old clients keep the behaviour they had.
func TestManager_RollSchedules_ZeroMaskStillRollsDaily(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	m.SetLocation(stockholm(t))
	m.SetSchedule("garage", Schedule{SoC: 0.8, TimeOfDayMinUTC: 360, Recurring: true})

	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC) // Friday evening
	m.RollSchedules(now)

	st, _ := m.State("garage")
	want := time.Date(2026, 5, 16, 6, 0, 0, 0, time.UTC) // Saturday
	if !st.TargetTime.Equal(want) {
		t.Fatalf("zero mask: target_time = %v, want Saturday %v", st.TargetTime, want)
	}
}

// SetSchedule drops a stray high bit rather than storing it: the mask
// is 7 bits and the eighth would silently confuse the roll.
func TestSetSchedule_MasksDaysToSevenBits(t *testing.T) {
	m := NewManager()
	m.Load([]Config{{ID: "garage", DriverName: "easee"}})
	m.SetSchedule("garage", Schedule{
		SoC: 0.8, TimeOfDayMinUTC: 360, Recurring: true, Days: 0x80 | maskMonToFri,
	})
	got, ok := m.GetSchedule("garage")
	if !ok {
		t.Fatal("schedule not stored")
	}
	if got.Days != maskMonToFri {
		t.Fatalf("days = %#x, want the high bit dropped (%#x)", got.Days, maskMonToFri)
	}
}

// A mask with nothing to mask is no intent: Days alone does not make
// a schedule non-Empty, so it persists (and reads back) as cleared.
func TestScheduleEmpty_DaysAloneIsNoIntent(t *testing.T) {
	if !(Schedule{Days: maskMonToFri}).Empty() {
		t.Error("a days-only Schedule should be Empty — there is nothing to schedule")
	}
}
