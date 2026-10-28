package api

import (
	"testing"
	"time"
)

func TestCalendarMonthBoundsUsesLocalCalendarBoundaries(t *testing.T) {
	tests := []struct {
		name, month, zone string
		now               time.Time
		wantDays          int
		wantHours         float64
	}{
		{"February common year", "2025-02", "UTC", time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC), 28, 672},
		{"February leap year", "2024-02", "UTC", time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC), 29, 696},
		{"31 day month", "2025-01", "UTC", time.Date(2025, 2, 1, 12, 0, 0, 0, time.UTC), 31, 744},
		{"DST spring month", "2025-03", "America/New_York", time.Date(2025, 4, 1, 12, 0, 0, 0, mustLoadLocation(t, "America/New_York")), 31, 743},
		{"DST autumn month", "2025-11", "America/New_York", time.Date(2025, 12, 1, 12, 0, 0, 0, mustLoadLocation(t, "America/New_York")), 30, 721},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := mustLoadLocation(t, tt.zone)
			now := tt.now.In(loc)
			start, end, err := calendarMonthBounds(tt.month, now)
			if err != nil {
				t.Fatal(err)
			}
			if start.Location() != loc || end.Location() != loc || start.Day() != 1 || start.Hour() != 0 || end.Day() != 1 || end.Hour() != 0 {
				t.Fatalf("bounds = %s to %s, want local midnight boundaries", start, end)
			}
			if got := start.AddDate(0, 1, -1).Day(); got != tt.wantDays {
				t.Fatalf("days = %d, want %d", got, tt.wantDays)
			}
			if got := end.Sub(start).Hours(); got != tt.wantHours {
				t.Fatalf("elapsed hours = %.0f, want %.0f", got, tt.wantHours)
			}
		})
	}
}

func TestCalendarMonthBoundsRejectsInvalidAndFutureMonths(t *testing.T) {
	now := time.Date(2025, 10, 26, 12, 0, 0, 0, time.UTC)
	for _, month := range []string{"2025-1", "2025-13", "not-a-month", "2025-11"} {
		if _, _, err := calendarMonthBounds(month, now); err == nil {
			t.Errorf("calendarMonthBounds(%q) unexpectedly succeeded", month)
		}
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}
