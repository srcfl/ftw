package ocpp

import (
	"testing"

	"github.com/lorenzodonini/ocpp-go/ocpp1.6/core"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Core asks chargers for a heartbeat at half the 60 s watchdog unless the
// owner set one; a saved value wins.
func TestHeartbeatDefaultIsHalfTheWatchdog(t *testing.T) {
	for _, tc := range []struct{ set, want int }{{0, 30}, {60, 60}, {120, 120}} {
		c := Config{HeartbeatIntervalS: tc.set}
		c.Defaults()
		if c.HeartbeatIntervalS != tc.want {
			t.Fatalf("heartbeat_interval_s %d: got %d, want %d", tc.set, c.HeartbeatIntervalS, tc.want)
		}
		h := NewHandler(telemetry.NewStore(), c.HeartbeatIntervalS)
		conf, err := h.OnBootNotification("charger", &core.BootNotificationRequest{ChargePointVendor: "Rig", ChargePointModel: "One"})
		if err != nil || conf.Interval != tc.want {
			t.Fatalf("heartbeat_interval_s %d: boot asked for %d s (%v)", tc.set, conf.Interval, err)
		}
	}
}
