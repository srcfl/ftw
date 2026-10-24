package main

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestHistorySunDarknessForLiveLoad(t *testing.T) {
	sun := historySunOf(59.33, 18.07, true) // Stockholm
	cases := map[string]telemetry.Darkness{
		"2026-06-21T12:00:00+02:00": telemetry.DarknessUnknown,
		"2026-10-21T02:00:00+02:00": telemetry.DarknessNight,
	}
	for at, want := range cases {
		ts, _ := time.Parse(time.RFC3339, at)
		if got := sun.darkness(ts); got != want {
			t.Fatalf("%s: darkness %d, want %d", at, got, want)
		}
	}
	if (historySun{}).darkness(time.Now()) != telemetry.DarknessUnknown {
		t.Fatal("an unplaced site inferred darkness")
	}
	// Through an October evening: daylight, then twilight, then night, in order.
	start, _ := time.Parse(time.RFC3339, "2026-10-21T15:00:00+02:00")
	seen := []telemetry.Darkness{}
	for m := 0; m <= 300; m++ {
		d := sun.darkness(start.Add(time.Duration(m) * time.Minute))
		if len(seen) == 0 || seen[len(seen)-1] != d {
			seen = append(seen, d)
		}
	}
	if len(seen) != 3 || seen[0] != telemetry.DarknessUnknown || seen[1] != telemetry.DarknessTwilight || seen[2] != telemetry.DarknessNight {
		t.Fatalf("evening sequence %v", seen)
	}
}
