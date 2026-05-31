package main

import "testing"

// The site-meter owner must be EXCLUDED from the stale-meter DefaultMode
// sweep. On a combined meter+battery device (Pixii / Ferroamp-class), the
// meter going stale is usually its own hung modbus poll; writing DefaultMode
// into that same session flaps the battery in/out every minute. The
// "don't act on a stale grid signal" protection applies to the OTHER
// batteries, not the meter owner. Regression guard for the anti-flap skip
// (removed in PR #405, restored here).
func TestDriversToDefaultOnSiteMeterStale_SkipsMeterOwner(t *testing.T) {
	got := driversToDefaultOnSiteMeterStale([]string{"ferroamp", "sungrow", "pixii"}, "pixii")
	want := []string{"ferroamp", "sungrow"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	for _, n := range got {
		if n == "pixii" {
			t.Fatalf("site-meter owner pixii must be skipped, got %v", got)
		}
	}
}

// When the site meter is a standalone device (not also a controllable
// driver), nothing is skipped — every driver is reverted to DefaultMode.
func TestDriversToDefaultOnSiteMeterStale_NoMeterOwnerInSet(t *testing.T) {
	got := driversToDefaultOnSiteMeterStale([]string{"ferroamp", "sungrow"}, "standalone_meter")
	if len(got) != 2 {
		t.Fatalf("expected both drivers defaulted, got %v", got)
	}
}
