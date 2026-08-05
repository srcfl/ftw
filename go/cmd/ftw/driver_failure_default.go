package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// The invariant this file serves: "a failed/stale driver receives its
// autonomous default mode." The watchdog covers stale. This covers failed —
// a driver that answers every poll but cannot put power where core asked.
//
// Two conditions reach it, and they are the same condition seen from
// opposite ends of the wire:
//
//   - the driver says so, via host.set_device_fault: a Ferroamp EnergyHub
//     in Fault Mode with its relays open, a Pixii mid-calibration;
//   - core sees it, because the driver refused the commands core sent.
//     A driver that answers polls and rejects writes stays Status=ok, so
//     nothing else notices; it sits in the dispatch set and in the MPC
//     fleet holding whatever setpoint it last accepted, and the power the
//     plan is counting on silently becomes grid import.
//
// Both land in telemetry as DeviceFault, so IsOnline() — the predicate the
// dispatcher and the planner already share — drops the driver from both.
// This tracker's own job is the other half: walk it to its declared default
// exactly once per transition, and re-arm when it can actuate again.
//
// Only dispatch commands are counted, never the default release itself. A
// driver that rejects the default is reporting that it was never under
// control — see set_self_consumption in sungrow.lua, which returns the
// default as held rather than failed for exactly that case — and nothing
// here escalates on it.
//
// Every path that dispatches feeds this, because a charger and a PV inverter
// go silently wrong the same way a battery does. Which command on a path
// counts is decided where that path lives, and every choice answers one
// question: does refusing this say core cannot put power where it asked?
//
//   - Storage sends the `battery` setpoint. main.go's dispatch loop.
//   - PV curtail sends the `curtail` cap, which counts, and the
//     `curtail_disable` release, which does not. pv_curtail_dispatch.go.
//   - The loadpoint sends the periodic `ev_set_current`, which counts, and
//     four other things that do not. See loadpoint.DispatchOutcomeFunc.

const (
	// driverRefusalLimit is how many refused dispatch commands in a row
	// mark a driver as unable to actuate. Three, matching
	// DriverHealth.RecordError's degrade threshold: a single rejected
	// Modbus write is a normal event on a busy device, three in a row at
	// control cadence is not.
	driverRefusalLimit = 3

	// driverRefusalRetryInterval is how long an excluded driver stays out
	// before one command is let through to test it again. A refusal is not
	// always permanent — an inverter can reject writes through a firmware
	// restart or a grid-code ride-through and take them again afterwards —
	// so the exclusion must not need an operator to end it. Long enough
	// that a device that stays broken sees one command per five minutes
	// rather than one per second.
	driverRefusalRetryInterval = 5 * time.Minute

	// driverCannotActuateReason labels the default requests this tracker
	// makes, next to the watchdog's "watchdog" and the freshness gate's
	// site_meter_stale.
	driverCannotActuateReason = "driver_cannot_actuate"
)

// driverActuationTracker emits one DefaultMode request per driver that has
// stopped being able to actuate, and re-arms when it recovers. Same shape as
// staleSiteDefaultTracker, which does this per site-meter transition; this
// one is per driver.
//
// Not safe for concurrent use: both methods run on the control-loop
// goroutine, recordCommandOutcome during dispatch and update at the top of
// the following tick.
type driverActuationTracker struct {
	tel       *telemetry.Store
	refusals  map[string]refusalState
	defaulted map[string]struct{}
}

type refusalState struct {
	consecutive int
	reason      string
	// excludedAt is when the driver was last put out of dispatch, and
	// zero while it is still in. It dates the retry window.
	excludedAt time.Time
}

func newDriverActuationTracker(tel *telemetry.Store) *driverActuationTracker {
	return &driverActuationTracker{
		tel:       tel,
		refusals:  map[string]refusalState{},
		defaulted: map[string]struct{}{},
	}
}

// dispatchCommand sends one dispatch command and files what the driver made
// of it. Wraps sendDriverCommand so the dispatch loop has one call, not two.
func (t *driverActuationTracker) dispatchCommand(
	ctx context.Context,
	reg driverCommandSender,
	kind, name string,
	payload []byte,
	timeout time.Duration,
	now time.Time,
) {
	t.recordCommandOutcome(name, sendDriverCommand(ctx, reg, kind, name, payload, timeout), now)
}

// releaseCommand sends a command that hands a device back to its own control
// — a curtail release — and files no outcome. It exists so the two halves of
// a dispatch path read as a pair at the call site, rather than one of them
// quietly dropping an error.
//
// This is the same rule dispatchCommand's doc names, seen from the other end:
// only dispatch commands are counted, never the release. A driver that
// rejects a release is reporting that it was never under control, and there
// is a second reason here beyond that principle — ComputePVCurtail sends
// `curtail_disable` to a driver the moment it drops offline, so counting a
// refused release would let an excluded driver hold itself out on the
// strength of its own exclusion, with no way back.
func (t *driverActuationTracker) releaseCommand(
	ctx context.Context,
	reg driverCommandSender,
	kind, name string,
	payload []byte,
	timeout time.Duration,
) {
	_ = sendDriverCommand(ctx, reg, kind, name, payload, timeout)
}

// recordCommandOutcome files the result of one dispatch command. Only a
// refusal counts; see isCommandRefusal for what does not.
func (t *driverActuationTracker) recordCommandOutcome(name string, err error, now time.Time) {
	if t == nil || t.tel == nil {
		return
	}
	if err == nil {
		if _, tracked := t.refusals[name]; tracked {
			delete(t.refusals, name)
			t.tel.SetDriverCommandFault(name, false, "")
		}
		return
	}
	if !isCommandRefusal(err) {
		return
	}
	if t.refusals == nil {
		t.refusals = map[string]refusalState{}
	}
	st := t.refusals[name]
	st.consecutive++
	st.reason = err.Error()
	if st.consecutive >= driverRefusalLimit && st.excludedAt.IsZero() {
		st.excludedAt = now
		t.tel.SetDriverCommandFault(name, true, st.reason)
	}
	t.refusals[name] = st
}

// update walks every driver that cannot actuate to its autonomous default,
// once per transition, and returns the names to send it to. It also ends the
// retry window for drivers excluded long enough to deserve another try.
//
// observeOnly drivers are never returned. A telemetry-only driver must
// receive no command at all, and that includes the safe one: core has no
// mandate over a device it was only asked to watch, and the "default" it
// would send is a write to an inverter somebody else is controlling.
func (t *driverActuationTracker) update(now time.Time, observeOnly map[string]bool) []string {
	if t == nil || t.tel == nil {
		return nil
	}
	health := t.tel.AllHealth()

	for name, st := range t.refusals {
		if _, known := health[name]; !known {
			// Driver removed or restarted: its health record went with
			// it, and so did the fault. Start it over clean.
			delete(t.refusals, name)
			delete(t.defaulted, name)
			continue
		}
		if st.excludedAt.IsZero() || now.Sub(st.excludedAt) < driverRefusalRetryInterval {
			continue
		}
		// Retry window is up. Let one command through: a single fresh
		// refusal puts the driver straight back out, an accepted one
		// clears the record entirely.
		st.consecutive = driverRefusalLimit - 1
		st.excludedAt = time.Time{}
		t.refusals[name] = st
		t.tel.SetDriverCommandFault(name, false, "")
		// health holds copies, so apply the release to this tick's
		// snapshot too — otherwise the latch below reads the driver as
		// still faulted and defaults a driver we just let back in.
		if h, ok := health[name]; ok {
			h.SetCommandFault(false, "")
			health[name] = h
		}
		slog.Info("driver exclusion retry window elapsed — letting one command through",
			"driver", name, "window", driverRefusalRetryInterval)
	}

	var pending []string
	for name, h := range health {
		if !h.DeviceFault {
			continue
		}
		if observeOnly[name] {
			continue
		}
		if h.Status == telemetry.StatusOffline {
			// Stale is the watchdog's transition to own, and it has
			// already sent this driver its default this tick.
			continue
		}
		if _, done := t.defaulted[name]; done {
			continue
		}
		if t.defaulted == nil {
			t.defaulted = map[string]struct{}{}
		}
		t.defaulted[name] = struct{}{}
		pending = append(pending, name)
		slog.Warn("driver cannot actuate — reverting it to its autonomous default",
			"driver", name, "reason", h.DeviceFaultReason)
	}

	for name := range t.defaulted {
		h, known := health[name]
		if known && h.DeviceFault {
			continue
		}
		delete(t.defaulted, name)
		if known {
			slog.Info("driver can actuate again — back in dispatch", "driver", name)
		}
	}

	sort.Strings(pending)
	return pending
}

// isCommandRefusal separates "this device rejected the write" from the other
// ways a Send can fail. Each exclusion is a fault somebody else already owns,
// and counting it here would book it twice:
//
//   - observe_only: the registry refused on the driver's behalf and never
//     touched the device. It says nothing about the hardware;
//   - control blocked: the registry is already holding this driver in its
//     default and retrying with backoff;
//   - deadline/cancel: a driver wedged in device I/O stops emitting
//     telemetry too, and the staleness watchdog walks it to its default.
//     Same reasoning as sendDriverCommand's timeout handling — a slow cloud
//     driver must not lose control over one bad round trip.
//
// ErrCommandMayHaveRun is not an exclusion: the registry wraps a genuine
// refusal in it, and errors.Is sees the cause through Unwrap.
func isCommandRefusal(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, drivers.ErrObserveOnly):
		return false
	case errors.Is(err, drivers.ErrControlBlocked):
		return false
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return false
	}
	return true
}
