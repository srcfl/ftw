package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// pollingStore stands up a driver that is answering polls normally: fresh
// telemetry, Status ok, nothing the staleness watchdog would ever act on.
func pollingStore(t *testing.T, names ...string) *telemetry.Store {
	t.Helper()
	tel := telemetry.NewStore()
	for _, name := range names {
		soc := 0.5
		tel.Update(name, telemetry.DerBattery, 0, &soc, nil)
		tel.DriverHealthMut(name).RecordSuccess()
	}
	return tel
}

// Hole (a): a driver that polls fine and flags DeviceFault produced no
// watchdog transition — WatchdogScan keys on LastSuccess only — so it was
// dropped from dispatch and then held its last setpoint forever. Nothing
// ever asked it for its declared safe state.
func TestDeviceFaultReachesDefaultOncePerTransition(t *testing.T) {
	tel := pollingStore(t, "ferroamp")
	tracker := newDriverActuationTracker(tel)
	now := time.Now()

	if pending := tracker.update(now, nil); len(pending) != 0 {
		t.Fatalf("healthy driver was sent a default: %v", pending)
	}

	tel.SetDriverDeviceFault("ferroamp", true, "EnergyHub Fault Mode")
	assertStringsEqual(t, tracker.update(now, nil), []string{"ferroamp"})

	// The driver re-asserts the fault on every poll. That must not turn
	// into one default command per control tick.
	for i := 0; i < 5; i++ {
		now = now.Add(time.Second)
		tel.SetDriverDeviceFault("ferroamp", true, "EnergyHub Fault Mode")
		if pending := tracker.update(now, nil); len(pending) != 0 {
			t.Fatalf("tick %d re-sent the default: %v", i, pending)
		}
	}

	// Recovery re-arms the latch; the next fault is protected again.
	tel.SetDriverDeviceFault("ferroamp", false, "")
	if pending := tracker.update(now.Add(time.Second), nil); len(pending) != 0 {
		t.Fatalf("recovery sent a default: %v", pending)
	}
	tel.SetDriverDeviceFault("ferroamp", true, "EnergyHub Fault Mode again")
	assertStringsEqual(t, tracker.update(now.Add(2*time.Second), nil), []string{"ferroamp"})
}

// A stale driver is the watchdog's transition. It has already sent that
// driver its default this tick; the tracker must not send a second one.
func TestStaleDriverIsLeftToTheWatchdog(t *testing.T) {
	tel := pollingStore(t, "ferroamp")
	tracker := newDriverActuationTracker(tel)
	now := time.Now()

	tel.SetDriverDeviceFault("ferroamp", true, "EnergyHub Fault Mode")
	tel.WatchdogScan(time.Nanosecond) // walk it offline the way staleness does

	if pending := tracker.update(now, nil); len(pending) != 0 {
		t.Fatalf("stale driver got a second default: %v", pending)
	}
}

// An observe_only driver must receive no command at all, and that includes
// the safe one — core has no mandate over a device it was only asked to
// watch.
func TestObserveOnlyDriverNeverReceivesADefault(t *testing.T) {
	tel := pollingStore(t, "neighbour-inverter")
	tracker := newDriverActuationTracker(tel)

	tel.SetDriverDeviceFault("neighbour-inverter", true, "fault mode")
	pending := tracker.update(time.Now(), map[string]bool{"neighbour-inverter": true})
	if len(pending) != 0 {
		t.Fatalf("observe_only driver was sent a default: %v", pending)
	}
}

// Hole (b): a driver that answers every poll and rejects every command kept
// Status=ok, so it stayed in the dispatch set and in the MPC fleet. The plan
// went on counting on power it never delivered, and the shortfall became
// grid import.
func TestRefusedCommandsExcludeDriverAndReachTheDefaultOnce(t *testing.T) {
	tel := pollingStore(t, "sungrow")
	tracker := newDriverActuationTracker(tel)
	refuse := &stubSender{handler: func(ctx context.Context, name string) error {
		return errors.New("modbus write refused")
	}}
	now := time.Now()
	payload := []byte(`{"action":"battery","power_w":-2000}`)

	// A single refusal is a normal event on a busy device.
	tracker.dispatchCommand(context.Background(), refuse, "driver send", "sungrow", payload, time.Second, now)
	if h := tel.DriverHealth("sungrow"); !h.IsOnline() {
		t.Fatal("one refused write dropped the battery out of control")
	}

	for i := 1; i < driverRefusalLimit; i++ {
		now = now.Add(time.Second)
		tracker.dispatchCommand(context.Background(), refuse, "driver send", "sungrow", payload, time.Second, now)
	}

	h := tel.DriverHealth("sungrow")
	if h.IsOnline() {
		t.Fatalf("driver refused %d commands in a row and is still counted as controllable",
			driverRefusalLimit)
	}
	// IsOnline is the predicate ComputeDispatch and the MPC fleet share, so
	// this is what drops it from both. Status stays ok: it is still talking.
	if h.Status != telemetry.StatusOk {
		t.Errorf("Status = %v, want ok — the driver is answering polls", h.Status)
	}
	if h.DeviceFaultReason == "" {
		t.Error("exclusion carries no operator-facing reason")
	}

	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})
	if pending := tracker.update(now.Add(time.Second), nil); len(pending) != 0 {
		t.Fatalf("a driver held out of dispatch was defaulted again: %v", pending)
	}
}

// The exclusion must not need an operator to end it: a device can reject
// writes through a firmware restart and take them again afterwards.
func TestExcludedDriverIsRetriedAndRecovers(t *testing.T) {
	tel := pollingStore(t, "sungrow")
	tracker := newDriverActuationTracker(tel)
	refusing := true
	sender := &stubSender{handler: func(ctx context.Context, name string) error {
		if refusing {
			return errors.New("modbus write refused")
		}
		return nil
	}}
	now := time.Now()
	payload := []byte(`{"action":"battery","power_w":-2000}`)

	for i := 0; i < driverRefusalLimit; i++ {
		tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
	}
	tracker.update(now, nil)
	if tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("driver was not excluded")
	}

	// Still inside the retry window.
	tracker.update(now.Add(driverRefusalRetryInterval-time.Second), nil)
	if tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("exclusion ended before the retry window")
	}

	now = now.Add(driverRefusalRetryInterval)
	tracker.update(now, nil)
	if !tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("retry window elapsed and the driver was not let back in")
	}

	// One more refusal puts it straight back out — no second grace period.
	tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
	if tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("a driver that refused its retry stayed in dispatch")
	}
	// And the fault re-armed, so it is walked to its default again.
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})

	// The device takes writes again: the next accepted command clears it.
	now = now.Add(driverRefusalRetryInterval)
	tracker.update(now, nil)
	refusing = false
	tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
	if !tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("driver accepted a command and is still excluded")
	}
}

// Every one of these is a fault somebody else already owns. Counting them
// here would book the same fault twice, and could push a merely slow cloud
// driver out of control on one bad round trip.
func TestNonRefusalErrorsDoNotExcludeADriver(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"observe only", drivers.ErrObserveOnly},
		{"control blocked", drivers.ErrControlBlocked},
		{"command deadline", context.DeadlineExceeded},
		{"tick cancelled", context.Canceled},
		{"wrapped deadline", &wrappedErr{cause: context.DeadlineExceeded}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if isCommandRefusal(tc.err) {
				t.Fatalf("%v counted as a refusal", tc.err)
			}
			tel := pollingStore(t, "cloud-battery")
			tracker := newDriverActuationTracker(tel)
			now := time.Now()
			for i := 0; i < driverRefusalLimit*2; i++ {
				tracker.recordCommandOutcome("cloud-battery", tc.err, now)
				now = now.Add(time.Second)
			}
			if !tel.DriverHealth("cloud-battery").IsOnline() {
				t.Fatalf("%v excluded the driver from control", tc.err)
			}
			if pending := tracker.update(now, nil); len(pending) != 0 {
				t.Fatalf("%v produced a default request: %v", tc.err, pending)
			}
		})
	}
}

type wrappedErr struct{ cause error }

func (e *wrappedErr) Error() string { return "command may have run: " + e.cause.Error() }
func (e *wrappedErr) Unwrap() error { return e.cause }

// A removed driver has no health record, and a late verdict about it must
// not create one — that would put a card back in the UI for a driver that
// is gone.
func TestRemovedDriverIsForgotten(t *testing.T) {
	tel := pollingStore(t, "sungrow")
	tracker := newDriverActuationTracker(tel)
	now := time.Now()
	for i := 0; i < driverRefusalLimit; i++ {
		tracker.recordCommandOutcome("sungrow", errors.New("modbus write refused"), now)
	}
	tracker.update(now, nil)

	tel.Remove("sungrow")
	tracker.recordCommandOutcome("sungrow", errors.New("driver \"sungrow\" not found"), now)
	if h := tel.DriverHealth("sungrow"); h != nil {
		t.Fatalf("a removed driver was resurrected in telemetry: %+v", h)
	}
	if pending := tracker.update(now.Add(time.Second), nil); len(pending) != 0 {
		t.Fatalf("removed driver was sent a default: %v", pending)
	}
	if _, tracked := tracker.refusals["sungrow"]; tracked {
		t.Error("removed driver left bookkeeping behind")
	}
}
