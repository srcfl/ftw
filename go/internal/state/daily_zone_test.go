package state

import (
	"context"
	"testing"
)

// A Docker box cached its days on UTC. Once the household zone is stored,
// those dates cover other hours, so they go and are computed again.
func TestANewHouseholdZoneClearsTheCachedDays(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if cleared, err := s.KeepDailyZone(ctx, "UTC"); err != nil || cleared {
		t.Fatalf("first zone: cleared=%v err=%v, want the rows kept", cleared, err)
	}
	if err := s.SaveDailyEnergy("2026-10-30", DayEnergy{LoadWh: 1, Intervals: 1}); err != nil {
		t.Fatal(err)
	}
	if cleared, err := s.KeepDailyZone(ctx, "UTC"); err != nil || cleared {
		t.Fatalf("same zone: cleared=%v err=%v", cleared, err)
	}
	if _, ok, err := s.LoadDailyEnergy("2026-10-30"); err != nil || !ok {
		t.Fatalf("same zone dropped the day: ok=%v err=%v", ok, err)
	}
	if cleared, err := s.KeepDailyZone(ctx, "Europe/Stockholm"); err != nil || !cleared {
		t.Fatalf("new zone: cleared=%v err=%v, want the days cleared", cleared, err)
	}
	if _, ok, err := s.LoadDailyEnergy("2026-10-30"); err != nil || ok {
		t.Fatalf("a UTC day survived the move to Stockholm: ok=%v err=%v", ok, err)
	}
	if cleared, err := s.KeepDailyZone(ctx, "Europe/Stockholm"); err != nil || cleared {
		t.Fatalf("after the move: cleared=%v err=%v", cleared, err)
	}
}
