package main

import (
	"os"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
	"github.com/srcfl/ftw/go/internal/localzone"
)

// Erik's car goal (31 October 2026): 100 % by 17:00 on Sunday, on a Docker
// box whose host clock is UTC. With the zone stored on the site, the box
// plans 17:00 in Stockholm, 16:00Z in winter, instead of 17:00Z (18:00 at
// home).
func TestABoxOnUTCPlansACarGoalOnTheStoredHouseholdClock(t *testing.T) {
	local := time.Local
	t.Setenv("TZ", os.Getenv("TZ"))
	t.Cleanup(func() {
		_ = localzone.Use("")
		time.Local = local
	})
	time.Local = time.UTC

	useHouseholdClock("Europe/Stockholm")

	const sunday = 1 << 6
	goal := loadpoint.NewSchedule(1, 17*60, true, sunday, false)
	saturday := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	got := goal.NextDeadlineUTC(saturday, nil)
	if want := time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("17:00 goal due %s, want %s (17:00 in Stockholm)", got.UTC().Format(time.RFC3339), want.Format(time.RFC3339))
	}
}
