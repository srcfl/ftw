package telemetry

import (
	"testing"
	"time"
)

// The fleet report's driver facts: polls and failed polls over the last 24
// clock hours, all drivers together, forgetting the hour a day back, and how
// much of the day the count covers.
func TestPollsLastDayCountsADayOfPollsAndErrors(t *testing.T) {
	s := NewStore()
	now := time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)
	s.born = now.Add(-30 * time.Hour)
	s.mu.Lock()
	for i := range 10 {
		s.countPollLocked(now.Add(-25*time.Hour), i < 5) // a day and an hour ago: gone
		s.countPollLocked(now.Add(-23*time.Hour), i < 2)
		s.countPollLocked(now, false)
	}
	s.mu.Unlock()
	if polls, errors, covered := s.PollsLastDay(now); polls != 20 || errors != 2 || covered != 24*time.Hour {
		t.Fatalf("polls %d, errors %d, covered %s", polls, errors, covered)
	}
	if polls, _, _ := s.PollsLastDay(now.Add(48 * time.Hour)); polls != 0 {
		t.Fatalf("two days on, %d polls remain", polls)
	}
	s.born = now.Add(-2 * time.Hour)
	if _, _, covered := s.PollsLastDay(now); covered != 2*time.Hour {
		t.Fatalf("two hours after a restart the count covers %s", covered)
	}
}

// Only the poll loop counts. An OCPP charger's messages record success and
// errors on its health, but they are not polls, so a site whose one driver
// is OCPP has nothing to report rather than a 100 % failure from one
// faulted connector.
func TestOnlyPollsAreCounted(t *testing.T) {
	s := NewStore()
	s.RecordDriverError("ocpp-charger", "connector faulted")
	s.RecordDriverSuccess("ocpp-charger")
	s.RecordDriverTick("inverter")
	if polls, errors, _ := s.PollsLastDay(time.Now()); polls != 0 || errors != 0 {
		t.Fatalf("health records were counted as polls: %d %d", polls, errors)
	}
	s.CountPoll(false)
	s.CountPoll(true)
	if polls, errors, _ := s.PollsLastDay(time.Now()); polls != 2 || errors != 1 {
		t.Fatalf("polls %d, errors %d", polls, errors)
	}
}
