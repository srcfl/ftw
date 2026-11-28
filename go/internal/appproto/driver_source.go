package appproto

import (
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// DriverSource is a driver's row in the source table, read from its health.
//
// staleAfter is the site's watchdog timeout. A driver's own timeout wins,
// because a P1 meter answering every second and a cloud-polled inverter
// answering every minute are not comparable. The row's last good reading is
// placed against uptimeMs, the box uptime at now.
func DriverSource(name string, h *telemetry.DriverHealth, staleAfter time.Duration, uptimeMs int64, now time.Time) Source {
	src := Source{ID: name, Kind: "driver", Name: name, LastOkUptimeMs: -1}
	if h == nil {
		return src
	}
	if h.WatchdogTimeoutOverride > 0 {
		staleAfter = h.WatchdogTimeoutOverride
	}
	src.StaleAfterMs = staleAfter.Milliseconds()
	src.Offline = !h.IsOnline()
	if h.LastSuccess != nil {
		src.LastOkUptimeMs = uptimeMs - now.Sub(*h.LastSuccess).Milliseconds()
	}
	return src
}

// DriverSourceState is the state a session's source table gives the driver
// at now. The box's own page reads it from /api/status, so the page and the
// apps call the same device live.
func DriverSourceState(h *telemetry.DriverHealth, staleAfter time.Duration, now time.Time) SourceState {
	// Only the age of the last good reading matters, so measure uptime
	// from that reading.
	var age int64
	if h != nil && h.LastSuccess != nil {
		age = max(now.Sub(*h.LastSuccess).Milliseconds(), 0)
	}
	return sourceState(DriverSource("", h, staleAfter, age, now), age)
}
