// Package events is a tiny synchronous pub/sub bus used to decouple core
// loops (control tick, API handlers, watchdog) from cross-cutting
// subscribers (notifications, future webhooks, future audit log).
//
// Producers call Publish with a typed event. Subscribers register per
// Event.Kind() string. The bus is intentionally synchronous: Publish
// runs every handler inline on the caller's goroutine, so handlers must
// not block on long work. A handler that needs to do slow I/O should
// spawn its own goroutine.
package events

import (
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Event is anything carrying a stable Kind string for dispatch.
type Event interface {
	Kind() string
}

// Handler is a subscribed callback.
type Handler func(Event)

// Bus is a fan-out pub/sub hub. The zero value is unusable — call NewBus.
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]Handler
}

// NewBus returns a ready bus.
func NewBus() *Bus { return &Bus{subs: map[string][]Handler{}} }

// Subscribe registers a handler for a given event kind. Safe from multiple
// goroutines. A nil bus or nil handler is a no-op.
func (b *Bus) Subscribe(kind string, h Handler) {
	if b == nil || h == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[kind] = append(b.subs[kind], h)
}

// Publish dispatches an event to every handler registered for its kind.
// A nil bus is a no-op so producers can be wired unconditionally.
func (b *Bus) Publish(e Event) {
	if b == nil || e == nil {
		return
	}
	b.mu.RLock()
	hs := append([]Handler(nil), b.subs[e.Kind()]...)
	b.mu.RUnlock()
	for _, h := range hs {
		h(e)
	}
}

// ---- event kinds ----

const (
	KindHealthTick              = "health.tick"
	KindDriverLost              = "driver.lost"
	KindDriverRecovered         = "driver.recovered"
	KindNotificationTest        = "notifications.test"
	KindNotificationDispatched  = "notifications.dispatched"
	KindUpdateAvailable         = "update.available"
	KindUpdateInstalled         = "update.installed"
	KindChargingConnected       = "charging.connected"
	KindChargingSessionComplete = "charging.session_complete"
	KindChargingInterrupted     = "charging.interrupted"
	KindChargingGoalAtRisk      = "charging.goal_at_risk"
	// KindCarWhich asks which car is on a charger (docs/cars.md).
	KindCarWhich = "car.which"
)

// HealthTick is fired every control-loop tick with the current telemetry
// driver-health snapshot. Subscribers use it for time-threshold logic
// (e.g. "fire an alert after N seconds of staleness") without coupling
// the core loop to their own state.
type HealthTick struct {
	Health map[string]telemetry.DriverHealth
	Now    time.Time
}

func (HealthTick) Kind() string { return KindHealthTick }

// DriverLost is fired from the watchdog when a driver flips offline.
// It carries enough identity for any downstream subscriber without
// requiring them to query telemetry again.
type DriverLost struct {
	Driver      string
	LastSuccess *time.Time
	At          time.Time
}

func (DriverLost) Kind() string { return KindDriverLost }

// DriverRecovered is fired when the watchdog observes a driver coming
// back online after having flipped to offline.
type DriverRecovered struct {
	Driver string
	At     time.Time
}

func (DriverRecovered) Kind() string { return KindDriverRecovered }

// NotificationDispatched is emitted by the notifications service after
// every publish attempt — success or failure. Subscribers (history
// log, future audit/webhook sinks) consume it without needing a direct
// reference to the notifications package.
type NotificationDispatched struct {
	Time      time.Time
	EventType string
	Driver    string
	Title     string
	Body      string
	Priority  int
	Status    string // "sent" | "failed" | "queued" (held by FTW Cloud's uplink)
	Error     string // empty unless Status=="failed"
}

func (NotificationDispatched) Kind() string { return KindNotificationDispatched }

// UpdateAvailable is emitted by the self-update checker when a new
// release is discovered (and not on the operator's skip list). Emitted
// at most once per unique Version so a periodic re-check doesn't spam
// subscribers that already saw the transition.
type UpdateAvailable struct {
	Version         string
	PreviousVersion string
	ReleaseNotesURL string
	PublishedAt     time.Time
	At              time.Time
}

func (UpdateAvailable) Kind() string { return KindUpdateAvailable }

// NotificationTest is emitted by the API "Send test" endpoint. Reply
// receives the dispatch result so the HTTP handler can surface any
// transport error to the operator.
type NotificationTest struct {
	Reply chan<- error
}

func (NotificationTest) Kind() string { return KindNotificationTest }

// UpdateInstalled is emitted once, on the first boot of a new version: the
// previous run wrote its version down, and this run reads a different one
// back. It is the "everything came back on its own" moment, which is the
// only update fact worth a lock screen.
type UpdateInstalled struct {
	Version         string
	PreviousVersion string
	At              time.Time
}

func (UpdateInstalled) Kind() string { return KindUpdateInstalled }

// ChargingSessionComplete is emitted once per session when the active target
// was reached: by a fresh, matched vehicle BMS reading, or, for a car that
// reports nothing itself, by the box's estimate from a level someone set.
// The estimate is energy the box measured, so subscribers say it in kWh. A
// charger refusing current does not establish battery state.
type ChargingSessionComplete struct {
	LoadpointID string
	KWh         float64 // what the session meter delivered
	At          time.Time
}

func (ChargingSessionComplete) Kind() string { return KindChargingSessionComplete }

// ChargingInterrupted is emitted when a session that had charged steadily
// for at least ten minutes stops without finishing while the cable is still
// in. The hysteresis lives at the emitter: a flapping cable never produces
// this event, so subscribers may treat every one as real.
type ChargingInterrupted struct {
	LoadpointID string
	At          time.Time
}

func (ChargingInterrupted) Kind() string { return KindChargingInterrupted }

// ChargingConnected names a confirmed cable connection, not a charge start.
type ChargingConnected struct {
	LoadpointID string
	At          time.Time
}

func (ChargingConnected) Kind() string { return KindChargingConnected }

// ChargingGoalAtRisk is emitted once per plug-in when the plan cannot bring
// the car to its target by the ready time, and the box knows the car's
// level rather than assuming it.
type ChargingGoalAtRisk struct {
	LoadpointID string
	TargetSoC   float64 // fraction
	By          time.Time
	// ByClock is By as a household reads it, "07:00" in the box's zone.
	ByClock     string
	ShortfallWh float64
	At          time.Time
}

func (ChargingGoalAtRisk) Kind() string { return KindChargingGoalAtRisk }

// CarWhich is emitted once per plug-in when the box cannot tell which of its
// cars is on a charger. Session names the plug-in a tap answers, and CarIDs
// are the cars a tap may pick, in the box's order: at most four, empty when
// there are more.
type CarWhich struct {
	LoadpointID string
	Session     string
	CarIDs      []string
	At          time.Time
}

func (CarWhich) Kind() string { return KindCarWhich }
