package appproto

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The box's page and the apps grade a driver with the same ladder, so a
// device the page calls quiet is one the apps call quiet too.
func TestDriverSourceStateFollowsTheLadder(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	cases := []struct {
		name string
		h    *telemetry.DriverHealth
		want SourceState
	}{
		{"no health at all", nil, SourceNever},
		// A driver on a wrong port has polled and failed, but never read.
		{"never answered", &telemetry.DriverHealth{ConsecutiveErrors: 3}, SourceNever},
		{"answered just now", &telemetry.DriverHealth{LastSuccess: ago(2 * time.Second)}, SourceLive},
		{"past its timeout", &telemetry.DriverHealth{LastSuccess: ago(90 * time.Second)}, SourceLagging},
		{"long gone", &telemetry.DriverHealth{LastSuccess: ago(10 * time.Minute)}, SourceDown},
		{"watchdog took it offline", &telemetry.DriverHealth{LastSuccess: ago(2 * time.Second), Status: telemetry.StatusOffline}, SourceDown},
		{"device fault", &telemetry.DriverHealth{LastSuccess: ago(2 * time.Second), DeviceFault: true}, SourceDown},
		// A cloud-polled device answers once a few minutes; its own timeout
		// decides, not the site's.
		{"slow driver inside its own timeout", &telemetry.DriverHealth{LastSuccess: ago(3 * time.Minute), WatchdogTimeoutOverride: 5 * time.Minute}, SourceLive},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DriverSourceState(c.h, time.Minute, now); got != c.want {
				t.Fatalf("state = %q, want %q", got, c.want)
			}
		})
	}
}

// The row an app session sends grades to the same state the page reads.
func TestDriverSourceRowMatchesDriverSourceState(t *testing.T) {
	now := time.Now()
	last := now.Add(-75 * time.Second)
	h := &telemetry.DriverHealth{LastSuccess: &last}
	const uptimeMs = 3_600_000

	src := DriverSource("sungrow", h, time.Minute, uptimeMs, now)
	if src.ID != "sungrow" || src.Kind != "driver" || src.LastOkUptimeMs != uptimeMs-75_000 || src.StaleAfterMs != 60_000 {
		t.Fatalf("row = %+v", src)
	}
	if got, want := sourceState(src, uptimeMs), DriverSourceState(h, time.Minute, now); got != want {
		t.Fatalf("session grades %q, page grades %q", got, want)
	}
}
