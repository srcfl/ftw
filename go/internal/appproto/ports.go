package appproto

import (
	"context"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
)

// UptimeClock is the only clock this protocol trusts for ages.
//
// A Raspberry Pi has no RTC. After a power cut its wall clock reads 1970 until
// NTP answers, so every freshness claim measured against it would be wrong by
// decades — and freshness is the one thing the app exists to be honest about.
// Wall clock appears on the wire twice: the clock sync record, which says how
// much to trust it, and plan slot start times, which are future moments a
// person reads off a screen.
type UptimeClock interface {
	// UptimeMs is milliseconds since the box started. Monotonic.
	UptimeMs() int64
	// Now is wall clock.
	Now() time.Time
	// Sync describes the wall clock's provenance: source name and when it
	// was last synchronised.
	Sync() (source string, syncedAtMs int64)
}

// SystemClock is the ordinary implementation.
type SystemClock struct {
	// StartedAt anchors uptime. Wall clock is fine for this because only
	// differences are ever taken from it.
	StartedAt time.Time
	// Source names where the wall clock came from: "ntp", "rtc", "none".
	Source     string
	SyncedAtMs int64
}

func (c SystemClock) UptimeMs() int64 { return time.Since(c.StartedAt).Milliseconds() }
func (c SystemClock) Now() time.Time  { return time.Now() }
func (c SystemClock) Sync() (string, int64) {
	source := c.Source
	if source == "" {
		source = "none"
	}
	return source, c.SyncedAtMs
}

// Source is one device's freshness as the box knows it.
type Source struct {
	ID   string
	Kind string
	Name string
	// LastOkUptimeMs is box uptime at the last good reading. Negative means
	// the device has never answered.
	LastOkUptimeMs int64
	// StaleAfterMs is how long a reading from this device stays current. It
	// is the device's own cadence plus headroom, not a global constant: a
	// P1 meter and a cloud-polled inverter are not comparable.
	StaleAfterMs int64
	// Offline is set when the driver itself reports the device unreachable,
	// which is a stronger statement than an old reading.
	Offline bool
}

// Snapshot is the live site state the message layer reads.
//
// Powers follow the site convention: positive is into the site. PV is
// therefore negative while generating and the battery is positive while
// charging. No sign conversion happens in this package; that belongs at the
// driver boundary and nowhere else.
type Snapshot struct {
	Mode control.Mode
	// ControlRev is monotonic and bumped by every mutation of controllable
	// state. A command carries the rev it expected to act against.
	ControlRev uint64

	GridW    float64
	PVW      float64
	BatteryW float64
	LoadW    float64
	// BatterySoC is a fraction in [0,1], per the SI rule. The wire carries
	// permille, converted at the edge of this package.
	BatterySoC float64
	// BatterySoCKnown is false when no battery reports a state of charge.
	BatterySoCKnown bool

	// EVW is the summed charger draw, positive while charging — a charger
	// consumes like any other load. EVWKnown is false on a site with no
	// charger at all, and then field 10 is never sent: the app draws no EV
	// node rather than a dead one showing an invented zero.
	EVW      float64
	EVWKnown bool

	Sources []Source

	// DispatchBlockedBy names the source ids currently stopping dispatch.
	// It is supplied rather than derived: the dispatch gate in control is
	// the authority on its own safety rule, and a second implementation
	// here would be a second thing to keep in step.
	DispatchBlockedBy []string
}

// SiteReader hands the message layer the current state.
//
// Called once per telemetry tick and again at the dispatch boundary, which is
// what makes "guards are re-evaluated against fresh state" true rather than
// aspirational.
type SiteReader interface {
	Snapshot() Snapshot
}

// Identity is what the box calls itself.
type Identity struct {
	ID    string
	Build string
	TZ    string
}

// BoxInfoReader supplies identity and boot progress.
type BoxInfoReader interface {
	Identity() Identity
	// Boot returns non-nil while the box is still starting, and nil once it
	// can serve telemetry.
	Boot() *BootProgress
}

// ModeController applies a mode and reads it back.
//
// SetMode goes through control's existing validation. ObservedMode is the
// separate question of what the box is actually running, and the two are kept
// apart deliberately: cmd.result reports the read-back, never the request.
type ModeController interface {
	SetMode(ctx context.Context, m control.Mode) error
	ObservedMode() (control.Mode, bool)
}

// PlanReader hands over the planner's current output.
//
// Nil means the planner has produced nothing at all, which the wire reports as
// a stale plan with no slots rather than as an empty schedule.
type PlanReader interface {
	Latest() *mpc.Plan
	// Rev is bumped whenever the box replans, so the app can tell a fresh
	// plan from a redelivery of the same one.
	Rev() uint64
	// CeilingW is the import ceiling being defended, nil when there is none.
	CeilingW() *int64
}

// Sender delivers an encoded frame to the carrier.
type Sender interface {
	Send(frame []byte) error
}
