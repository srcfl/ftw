package api

import (
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// For a one-phase car the command's power is a third of the order.
// "Charging on one phase" explains a draw that matches it; a limit, a
// changed setpoint or a response still due says more and comes first, and
// a draw beyond tolerance is judged as for any car.
func TestOnePhaseVerdictComesAfterLimits(t *testing.T) {
	now := time.Now()
	connected := true
	for _, tc := range []struct {
		name                  string
		marked                bool
		core                  string
		sent, actual          float64
		requestedA, offeredA  float64
		mismatch, over        bool // since 12 minutes
		gap                   float64
		observedBeforeCommand bool
		readbackMismatch      bool
		state, reason         string
	}{
		{"fuse-limited order, car takes its third", true, "fuse_limit", 1380, 1380, 6, 6, false, false, 0, false, false, "limited", "fuse_limit"},
		{"same, unmarked car taking the order", false, "fuse_limit", 4140, 4140, 6, 6, false, false, 0, false, false, "limited", "fuse_limit"},
		{"import-limited, car takes its third", true, "import_limit", 1610, 1600, 7, 7, false, false, 0, false, false, "limited", "import_limit"},
		{"fuse cooldown, car takes its third", true, "fuse_cooldown", 1380, 1380, 6, 6, false, false, 0, false, false, "limited", "fuse_cooldown"},
		{"meter stale, car takes its third", true, "site_meter_stale", 1380, 1380, 6, 6, false, false, 0, false, false, "limited", "site_meter_stale"},
		{"setpoint changed by another app, draw matches", true, "", 3680, 3680, 16, 16, false, false, 0, false, true, "warning", "setpoint_changed"},
		{"reading older than the command", true, "", 2300, 2300, 10, 10, false, false, 0, true, false, "waiting", "waiting_response"},
		{"plan order, car takes its third", true, "", 3680, 3650, 16, 16, false, false, 0, false, false, "limited", "one_phase"},
		{"overdraw: charger ignores a cut 16->10 A", true, "", 2300, 3680, 10, 16, true, true, 1380, false, false, "warning", "power_above_target"},
		{"overdraw under fuse_limit", true, "fuse_limit", 2300, 3680, 10, 16, true, true, 1380, false, false, "warning", "power_above_target"},
		{"shortfall: car takes 1.5 of 3.7 kW", true, "", 3680, 1500, 16, 16, true, false, 2180, false, false, "warning", "power_below_target"},
		{"wrong mark: 3p car draws the full order", true, "", 3680, 11040, 16, 16, true, true, 7360, false, false, "warning", "power_above_target"},
		{"overdraw inside tolerance (+300 W on 3.68 kW)", true, "", 3680, 3980, 16, 16, true, true, 300, false, false, "limited", "one_phase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := telemetry.CommandEvidence{Result: "accepted", Since: now.Add(-15 * time.Minute), LastObservation: now}
			if tc.mismatch {
				c.PowerMismatchSince = now.Add(-12 * time.Minute)
				c.LastGapW = &tc.gap
			} else {
				c.PowerMatchSince = now.Add(-time.Minute)
			}
			if tc.over {
				c.PowerOverSince = now.Add(-12 * time.Minute)
			}
			if tc.readbackMismatch {
				c.ReadbackMismatchSince = now.Add(-time.Minute)
			}
			f := ControlFeedback{Kind: "ev", SentW: watts(tc.sent), ActualW: watts(tc.actual), ReadbackW: watts(tc.sent),
				ObservedAtMs: now.UnixMilli(), Response: "device_reported", RequestedA: watts(tc.requestedA), OfferedA: watts(tc.offeredA)}
			if tc.observedBeforeCommand {
				f.ObservedAtMs = now.Add(-20 * time.Minute).UnixMilli()
			}
			if tc.marked {
				f.ChargingPhase = "L1"
			}
			classifyControlFeedback(&f, c, true, true, tc.core, &connected, now)
			if f.State != tc.state || f.Reason != tc.reason {
				t.Fatalf("%s/%s, want %s/%s", f.State, f.Reason, tc.state, tc.reason)
			}
		})
	}
}
