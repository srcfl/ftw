package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/control"
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

// The registry closes its own command gate when it cannot confirm the safe
// default after a failed command. That is stronger evidence than the normal
// three-refusal heuristic and must reach the shared DriverHealth gate even
// when the returned error also carries the original command failure.
func TestRegistryControlBlockImmediatelyExcludesDriver(t *testing.T) {
	tel := pollingStore(t, "sungrow")
	tracker := newDriverActuationTracker(tel)
	now := time.Now()
	err := errors.Join(
		errors.New("modbus write refused"),
		errors.Join(drivers.ErrControlBlocked, errors.New("default mode write refused")),
	)

	tracker.recordCommandOutcome("sungrow", err, now)

	if tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("registry-blocked driver remained eligible for Core control")
	}
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})
}

// A hybrid inverter may accept its battery setpoint and reject its PV cap in
// the same tick. The accepted action must not reset the other action's streak.
func TestHybridActionSuccessDoesNotClearCurtailRefusals(t *testing.T) {
	tel := pollingStore(t, "hybrid")
	tracker := newDriverActuationTracker(tel)
	battery := &stubSender{}
	curtail := refusingSender("curtail register refused")
	now := time.Now()
	payload := []byte(`{"action":"battery","power_w":-2000}`)
	targets := []control.CurtailTarget{{Driver: "hybrid", LimitW: 2500}}

	for i := 0; i < driverRefusalLimit; i++ {
		tracker.dispatchCommand(context.Background(), battery, "driver send", "hybrid", payload, time.Second, now)
		dispatchPVCurtail(context.Background(), curtail, tracker, targets, time.Second, now)
		now = now.Add(time.Second)
	}

	if tel.DriverHealth("hybrid").IsOnline() {
		t.Fatal("accepted battery actions cleared the failed PV-curtail streak")
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
		{"EV health gate", drivers.ErrCommandIneligible},
		{"stale EV continuation", drivers.ErrCommandSuperseded},
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

// excludeDuringOutage refuses driverRefusalLimit battery commands while the
// device's reads are failing, the way a Sungrow refuses every write while it
// is unreachable, and lets the tracker send its one default.
func excludeDuringOutage(t *testing.T, tel *telemetry.Store, tracker *driverActuationTracker, sender driverCommandSender, now time.Time) {
	t.Helper()
	payload := []byte(`{"action":"battery","power_w":-3000}`)
	tel.RecordDriverError("sungrow", "driver_poll: none of 17 modbus reads returned data")
	for i := 0; i < driverRefusalLimit; i++ {
		tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
	}
	if tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("driver was not excluded")
	}
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})
}

// Random rig seeds 207532–207539 (27 Oct 2026): an inverter outage made the
// Sungrow refuse its battery command and its default, so it was excluded.
// When it came back it still ran its last forced command (187 / 3854 W in
// 207537, from 213 to 510 s) because Core neither resent the default nor
// let a command through until driverRefusalRetryInterval had passed.
func TestDriverExcludedDuringOutageGetsDefaultWhenItReadsAgain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		watchdog bool
	}{
		{"short outage, reads failing", false},
		{"long outage, watchdog offline", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := pollingStore(t, "sungrow")
			tracker := newDriverActuationTracker(tel)
			now := time.Now()
			excludeDuringOutage(t, tel, tracker, refusingSender("modbus reconnect backoff active"), now)

			now = now.Add(10 * time.Second)
			if tc.watchdog {
				tel.WatchdogScan(time.Nanosecond)
			}
			if pending := tracker.update(now, nil); len(pending) != 0 {
				t.Fatalf("unreachable driver got a second default: %v", pending)
			}

			// First fresh reading after the outage.
			now = now.Add(20 * time.Second)
			tel.RecordDriverSuccess("sungrow")
			assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})
			if !tel.DriverHealth("sungrow").IsOnline() {
				t.Fatal("driver back from an outage still waits out the retry window")
			}
			if pending := tracker.update(now.Add(time.Second), nil); len(pending) != 0 {
				t.Fatalf("returned driver got its default twice: %v", pending)
			}
		})
	}
}

// A device that keeps refusing while it keeps dropping out is retried each
// time it reads again. Once it refuses while reading fine, it waits the full
// retry window like any device that answers and refuses.
func TestDriverRefusingAfterOutageKeepsRetryWindowWhileOnline(t *testing.T) {
	tel := pollingStore(t, "sungrow")
	tracker := newDriverActuationTracker(tel)
	sender := refusingSender("modbus write refused")
	payload := []byte(`{"action":"battery","power_w":-3000}`)
	now := time.Now()
	excludeDuringOutage(t, tel, tracker, sender, now)

	// Back, but the trial command is refused while reads fail again.
	now = now.Add(5 * time.Second)
	tel.RecordDriverSuccess("sungrow")
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})
	tel.RecordDriverError("sungrow", "driver_poll: none of 17 modbus reads returned data")
	tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
	if tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("refused trial command left the driver in dispatch")
	}
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})

	// Back again: retried at once, not after the window.
	now = now.Add(5 * time.Second)
	tel.RecordDriverSuccess("sungrow")
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})
	if !tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("driver back from a second outage waits out the retry window")
	}

	// Now it reads fine and refuses: that is the device's answer.
	tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})
	excludedAt := now
	for i := 0; i < 5; i++ {
		now = now.Add(time.Minute - time.Second)
		// One failed read in between is not an outage.
		if i == 2 {
			tel.RecordDriverError("sungrow", "one modbus read timed out")
			if pending := tracker.update(now, nil); len(pending) != 0 {
				t.Fatalf("one failed read sent a default: %v", pending)
			}
		}
		tel.RecordDriverSuccess("sungrow")
		if pending := tracker.update(now, nil); len(pending) != 0 {
			t.Fatalf("online refusing driver was defaulted again at %v: %v", now.Sub(excludedAt), pending)
		}
		if tel.DriverHealth("sungrow").IsOnline() {
			t.Fatalf("online refusing driver let back in after %v", now.Sub(excludedAt))
		}
	}
	tracker.update(excludedAt.Add(driverRefusalRetryInterval), nil)
	if !tel.DriverHealth("sungrow").IsOnline() {
		t.Fatal("retry window elapsed and the driver was not let back in")
	}
}

// Rig seeds 207533 and 207536 (27 Oct 2026): the command hit the dead link
// before a poll did, so the last read had succeeded when the driver was
// excluded. In 207536 the outage lasted 29 s, under the 60 s watchdog, and
// the inverter held 187 / 3000 W for the full retry window after it came
// back. A failed read before any good one since the exclusion is the same
// outage.
func TestDriverExcludedJustBeforeShortOutageGetsDefaultWhenItReadsAgain(t *testing.T) {
	blocked := errors.Join(
		errors.New("driver command may have run: force command write failed: connection refused"),
		errors.Join(drivers.ErrControlBlocked, errors.New("driver_default_mode returned false")),
	)
	payload := []byte(`{"action":"battery","power_w":-3000}`)
	for _, tc := range []struct {
		name    string
		exclude func(*driverActuationTracker, time.Time)
	}{
		{"control blocked", func(tracker *driverActuationTracker, now time.Time) {
			tracker.recordCommandOutcomeFor("sungrow", "battery", blocked, now)
		}},
		{"refused three times", func(tracker *driverActuationTracker, now time.Time) {
			sender := refusingSender("force command write failed: connection refused")
			for i := 0; i < driverRefusalLimit; i++ {
				tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tel := pollingStore(t, "sungrow")
			tracker := newDriverActuationTracker(tel)
			now := time.Now()
			tc.exclude(tracker, now)
			assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})

			// Polls fail for 30 s, under the watchdog timeout.
			for i := 1; i <= 6; i++ {
				tel.RecordDriverError("sungrow", "driver_poll: none of 17 modbus reads returned data")
				tel.WatchdogScan(time.Minute)
				if pending := tracker.update(now.Add(time.Duration(i)*5*time.Second), nil); len(pending) != 0 {
					t.Fatalf("default sent while reads fail: %v", pending)
				}
				if tel.DriverHealth("sungrow").IsOnline() {
					t.Fatal("driver back in dispatch while reads fail")
				}
			}

			now = now.Add(35 * time.Second)
			tel.RecordDriverSuccess("sungrow")
			assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})
			if !tel.DriverHealth("sungrow").IsOnline() {
				t.Fatalf("driver back from a short outage waits out the %v window", driverRefusalRetryInterval)
			}
		})
	}
}

// A device that refuses for real on a flaky link: it is excluded with good
// reads, the next read fails, and it gets its one early trial on the good
// read after. It refuses that trial while reading fine. From then on a
// failed read between good ones is the link, not an outage, so it waits the
// full retry window instead of getting a default and a trial on every good
// read after a failed one.
func TestDriverRefusingOnFlakyLinkGetsOneEarlyTrial(t *testing.T) {
	tel := pollingStore(t, "sungrow")
	tracker := newDriverActuationTracker(tel)
	sender := refusingSender("modbus write refused")
	payload := []byte(`{"action":"battery","power_w":-3000}`)
	now := time.Now()
	for i := 0; i < driverRefusalLimit; i++ {
		tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
	}
	assertStringsEqual(t, tracker.update(now, nil), []string{"sungrow"})

	// Within one retry window of the first exclusion, the early trial
	// comes 4 s in and its window has not elapsed by the end.
	end := now.Add(driverRefusalRetryInterval)
	trials, defaults := 0, 0
	for now.Before(end) {
		now = now.Add(2 * time.Second)
		tel.RecordDriverError("sungrow", "one modbus read timed out")
		if pending := tracker.update(now, nil); len(pending) != 0 {
			t.Fatalf("default sent while a read failed: %v", pending)
		}
		now = now.Add(2 * time.Second)
		time.Sleep(time.Millisecond) // LastSuccess is wall time
		tel.RecordDriverSuccess("sungrow")
		defaults += len(tracker.update(now, nil))
		if tel.DriverHealth("sungrow").IsOnline() {
			trials++
			tracker.dispatchCommand(context.Background(), sender, "driver send", "sungrow", payload, time.Second, now)
			defaults += len(tracker.update(now, nil))
		}
	}
	if trials != 1 || defaults != 2 {
		t.Fatalf("refusing driver on a flaky link got %d trials and %d defaults inside the retry window, want 1 and 2", trials, defaults)
	}
}
