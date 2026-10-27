package api

import (
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"time"
)

func TestPositiveEVClampDoesNotHideNoPowerResponse(t *testing.T) {
	now := time.Now()
	connected := true
	for _, tc := range []struct {
		name, core   string
		sent, actual float64
		fresh        bool
		age          time.Duration
		want         string
	}{
		{"accepted ten amp offer, reason 78", "fuse_limit", 6900, 0, true, 3 * time.Minute, "no_power_response"},
		{"intentional zero", "fuse_limit", 0, 0, true, 3 * time.Minute, "fuse_limit"},
		{"still in grace", "fuse_limit", 6900, 0, true, time.Minute, "fuse_limit"},
		{"charging at limited rate", "fuse_limit", 6900, 6800, true, 3 * time.Minute, "fuse_limit"},
		{"stale charger", "fuse_limit", 6900, 0, false, 3 * time.Minute, "telemetry_stale"},
		{"stale site", "site_meter_stale", 6900, 0, true, 3 * time.Minute, "site_meter_stale"},
		{"cooldown", "fuse_cooldown", 6900, 0, true, 3 * time.Minute, "fuse_cooldown"},
		{"below the phase minimum", "below_phase_minimum", 0, 0, true, 3 * time.Minute, "below_phase_minimum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ControlFeedback{Kind: "ev", RequestedA: watts(15), OfferedA: watts(10), SentW: watts(tc.sent), ActualW: watts(tc.actual), ObservedAtMs: now.UnixMilli(), DeviceReason: "limited by dynamic charger current"}
			cmd := telemetry.CommandEvidence{Result: "accepted", Since: now.Add(-tc.age), NoPowerSince: now.Add(-tc.age), NoPowerAt: now}
			classifyControlFeedback(&f, cmd, true, tc.fresh, tc.core, &connected, now)
			setControlStatus(&f)
			if f.Reason != tc.want {
				t.Fatalf("got %+v want %s", f, tc.want)
			}
			if tc.want == "no_power_response" && f.Status != "not_following" {
				t.Fatalf("zero power must be visible: %+v", f)
			}
			if tc.want == "below_phase_minimum" && (f.Status != "limited" || f.Severity != "warning") {
				t.Fatalf("no valid order must ask for a look: %+v", f)
			}
		})
	}
}
