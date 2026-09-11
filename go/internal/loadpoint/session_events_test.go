package loadpoint

import (
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/events"
)

// The session-event latches. Each test drives Observe like the controller
// does — one call per tick — and asserts on what reaches the bus, because
// the bus is the only thing the notifications engine ever sees.

type eventLog struct {
	mu          sync.Mutex
	complete    []events.ChargingSessionComplete
	interrupted []events.ChargingInterrupted
}

func newEventLog(bus *events.Bus) *eventLog {
	log := &eventLog{}
	bus.Subscribe(events.KindChargingSessionComplete, func(e events.Event) {
		if ev, ok := e.(events.ChargingSessionComplete); ok {
			log.mu.Lock()
			log.complete = append(log.complete, ev)
			log.mu.Unlock()
		}
	})
	bus.Subscribe(events.KindChargingInterrupted, func(e events.Event) {
		if ev, ok := e.(events.ChargingInterrupted); ok {
			log.mu.Lock()
			log.interrupted = append(log.interrupted, ev)
			log.mu.Unlock()
		}
	})
	return log
}

func (l *eventLog) counts() (complete, interrupted int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.complete), len(l.interrupted)
}

type sessionRig struct {
	mgr *Manager
	log *eventLog
	now time.Time
}

func newSessionRig(t *testing.T) *sessionRig {
	t.Helper()
	r := &sessionRig{now: time.Unix(1_700_000_000, 0)}
	r.mgr = NewManager()
	r.mgr.Load([]Config{{ID: "garage", VehicleCapacityWh: 60_000}})
	r.mgr.SetNowFn(func() time.Time { return r.now })
	bus := events.NewBus()
	r.mgr.SetBus(bus)
	r.log = newEventLog(bus)
	return r
}

func (r *sessionRig) tick(d time.Duration, plugged bool, powerW, deliveredWh float64, requesting bool) {
	r.now = r.now.Add(d)
	r.mgr.Observe("garage", plugged, powerW, deliveredWh, requesting)
}

// The completion latch tripping is the publish point, and it publishes the
// session meter's own kWh — once, however long the finished car stays in.
func TestSessionCompletePublishesOnceWithSessionKWh(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetTarget("garage", 0.8, time.Time{})

	r.tick(0, true, 11_000, 0, true)
	r.tick(5*time.Minute, true, 11_000, 900, true)
	// Vehicle declines further charge; the latch needs the decline to hold
	// past SessionCompletionTimeout.
	r.tick(5*time.Minute, true, 0, 7_420, false)
	if c, _ := r.log.counts(); c != 0 {
		t.Fatal("latched before the timeout")
	}
	r.tick(SessionCompletionTimeout, true, 0, 7_420, false)
	if c, _ := r.log.counts(); c != 0 {
		t.Fatal("refusal invented a completed goal")
	}
	r.mgr.AnchorVehicleSoC("garage", .8)
	if c, _ := r.log.counts(); c != 1 {
		t.Fatalf("complete events = %d, want 1", c)
	}
	if got := r.log.complete[0]; got.KWh != 7.42 || got.LoadpointID != "garage" {
		t.Fatalf("event = %+v", got)
	}

	// Hours later, still plugged in, still declining: the latch holds.
	for i := 0; i < 10; i++ {
		r.tick(10*time.Minute, true, 0, 7_420, false)
	}
	if c, _ := r.log.counts(); c != 1 {
		t.Fatalf("complete events = %d after idle hours, want 1", c)
	}

	// A new session may speak again.
	r.tick(time.Minute, false, 0, 0, false)
	r.mgr.SetTarget("garage", 0.8, time.Time{})
	r.tick(time.Minute, true, 11_000, 0, true)
	r.tick(time.Minute, true, 0, 500, false)
	r.tick(SessionCompletionTimeout, true, 0, 500, false)
	r.mgr.AnchorVehicleSoC("garage", .8)
	if c, _ := r.log.counts(); c != 2 {
		t.Fatalf("complete events = %d after replug, want 2", c)
	}
}

// The interruption needs its full hysteresis: a steady run of at least ten
// minutes, then a confirmed stop the box did not order, cable still in.
func TestInterruptedFiresAfterSteadyRunAndConfirmedStop(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetCommandedW("garage", 11_000)

	r.tick(0, true, 11_000, 0, true)
	r.tick(InterruptSteadyRun, true, 11_000, 2_000, true) // run reaches 10 min
	r.tick(time.Second, true, 0, 2_000, true)             // charge dies, car still asking
	if _, i := r.log.counts(); i != 0 {
		t.Fatal("fired before the stop was confirmed")
	}
	r.tick(interruptConfirm, true, 0, 2_000, true)
	if _, i := r.log.counts(); i != 1 {
		t.Fatalf("interrupted events = %d, want 1", i)
	}

	// The stop persisting does not repeat the event: one stop, one fact.
	for i := 0; i < 10; i++ {
		r.tick(time.Minute, true, 0, 2_000, true)
	}
	if _, i := r.log.counts(); i != 1 {
		t.Fatalf("interrupted events = %d after idle, want 1", i)
	}
}

// A cable that flaps — short bursts of charging, none reaching the steady
// run — must not produce a single event, however long the night.
func TestFlappingCableStaysSilent(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetCommandedW("garage", 11_000)

	r.tick(0, true, 11_000, 0, true)
	for i := 0; i < 40; i++ {
		r.tick(2*time.Minute, true, 0, 500, true)      // dies after 2 min
		r.tick(3*time.Minute, true, 11_000, 500, true) // comes back
	}
	if _, i := r.log.counts(); i != 0 {
		t.Fatalf("interrupted events = %d from a flapping cable, want 0", i)
	}
}

// A pause the box ordered is the box working. The plan parking the car
// through expensive hours commands 0 W, and no phone may buzz for it.
func TestBoxOrderedPauseIsNotAnInterruption(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetCommandedW("garage", 11_000)

	r.tick(0, true, 11_000, 0, true)
	r.tick(InterruptSteadyRun, true, 11_000, 2_000, true)
	// The plan parks the charger: commanded 0, power dies.
	r.mgr.SetCommandedW("garage", 0)
	r.tick(time.Second, true, 0, 2_000, true)
	r.tick(interruptConfirm+time.Minute, true, 0, 2_000, true)
	r.tick(time.Hour, true, 0, 2_000, true)
	if _, i := r.log.counts(); i != 0 {
		t.Fatalf("interrupted events = %d for a plan pause, want 0", i)
	}

	// The plan resumes and the charger fails to come back: now it is real.
	r.mgr.SetCommandedW("garage", 11_000)
	r.tick(time.Minute, true, 0, 2_000, true)
	r.tick(interruptConfirm, true, 0, 2_000, true)
	if _, i := r.log.counts(); i != 1 {
		t.Fatalf("interrupted events = %d after a failed resume, want 1", i)
	}
}

// A vehicle that stopped asking chose to stop; that is the completion
// latch's story (or nobody's), never an interruption. And a completed
// session cannot also be an interrupted one.
func TestVehicleDeclineIsNotAnInterruption(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetTarget("garage", 0.8, time.Time{})
	r.mgr.SetCommandedW("garage", 11_000)

	r.tick(0, true, 11_000, 0, true)
	r.tick(InterruptSteadyRun, true, 11_000, 5_000, true)
	// Car reaches its limit: power dies AND the request drops.
	r.tick(time.Second, true, 0, 8_000, false)
	r.tick(interruptConfirm+SessionCompletionTimeout, true, 0, 8_000, false)
	c, i := r.log.counts()
	if c != 0 {
		t.Fatalf("complete events = %d, want 0", c)
	}
	if i != 0 {
		t.Fatalf("interrupted events = %d for a finished car, want 0", i)
	}
}

// Unplugging mid-stop is its own story (the cable is not still in), so the
// latch disarms rather than fires.
func TestPlugOutDisarmsTheInterruptionLatch(t *testing.T) {
	r := newSessionRig(t)
	r.mgr.SetCommandedW("garage", 11_000)

	r.tick(0, true, 11_000, 0, true)
	r.tick(InterruptSteadyRun, true, 11_000, 2_000, true)
	r.tick(time.Second, true, 0, 2_000, true)
	r.tick(30*time.Second, false, 0, 0, false) // unplugged before confirm
	r.tick(interruptConfirm, false, 0, 0, false)
	if _, i := r.log.counts(); i != 0 {
		t.Fatalf("interrupted events = %d after plug-out, want 0", i)
	}
}
