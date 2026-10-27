package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"sync"
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
//   - The solar feed sends the `solar_pv` hint, which does not count: a
//     refusal there says the owner has not opened the device-side gate
//     (the pump's Solar PV enable), not that core cannot put power where
//     it asked — no power was asked for. solar_feed_send.go.

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
// The loadpoint outcome runs inside the registry's per-driver actor so it can
// close health before the next queued EV command. mu keeps that short callback
// safe against a control-loop timeout moving on to update or another driver.
type driverActuationTracker struct {
	mu        sync.Mutex
	tel       *telemetry.Store
	refusals  map[string]refusalState
	defaulted map[string]struct{}
}

type refusalState struct {
	// consecutive is kept per dispatch action. Hybrid inverters can accept
	// their battery setpoint and reject their PV cap in the same tick; the
	// accepted battery command must not erase the curtail refusal streak.
	consecutive map[string]int
	reason      string
	// excludedAction is the action that crossed the refusal threshold. It is
	// left one refusal short when the retry window opens, so that action gets
	// exactly one trial without coupling its result to another action.
	excludedAction string
	// excludedAt is when the driver was last put out of dispatch, and
	// zero while it is still in. It dates the retry window.
	excludedAt time.Time
	// outage marks an exclusion that the device's own outage explains: its
	// reads were failing when it was excluded, or the watchdog took it
	// offline while it was out. Such a device kept the last command it
	// accepted and every default write in between failed. When it reads
	// again, update resends the default and lets one command through at
	// once instead of waiting out driverRefusalRetryInterval.
	outage bool
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
	t.recordCommandOutcomeFor(name, dispatchAction(payload), sendDriverCommand(ctx, reg, kind, name, payload, timeout), now)
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

// recordCommandOutcome is the loadpoint callback. The loadpoint controller
// reports only ev_set_current, so its action class is known at this boundary.
func (t *driverActuationTracker) recordCommandOutcome(name string, err error, now time.Time) {
	t.recordCommandOutcomeFor(name, "ev_set_current", err, now)
}

// recordCommandOutcomeFor files one dispatch result under its action class.
// Only a refusal counts; see isCommandRefusal for what does not.
func (t *driverActuationTracker) recordCommandOutcomeFor(name, action string, err error, now time.Time) {
	if t == nil || t.tel == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if action == "" {
		action = "dispatch"
	}
	if err == nil {
		if st, tracked := t.refusals[name]; tracked {
			// Once excluded, only update's timed retry may reopen the
			// driver. A later success from the same tick must not bypass it.
			if st.excludedAt.IsZero() || action != st.excludedAction {
				delete(st.consecutive, action)
			}
			if len(st.consecutive) == 0 {
				delete(t.refusals, name)
			} else {
				t.refusals[name] = st
			}
			// A different action may still own the driver-wide exclusion.
			// Only the action that failed can clear its own record.
			if st.excludedAt.IsZero() && len(st.consecutive) == 0 {
				t.tel.SetDriverCommandFault(name, false, "")
			}
		}
		return
	}
	if errors.Is(err, drivers.ErrControlBlocked) {
		// The registry has already closed its command gate because it could
		// not confirm the autonomous default. Reflect that authoritative
		// state in DriverHealth immediately. In particular, an error joined
		// from the original refusal and a failed default recovery must not be
		// dismissed merely because it also contains ErrControlBlocked.
		if t.refusals == nil {
			t.refusals = map[string]refusalState{}
		}
		st := t.refusals[name]
		if st.consecutive == nil {
			st.consecutive = map[string]int{}
		}
		st.consecutive[action] = driverRefusalLimit
		st.reason = err.Error()
		st.excludedAction = action
		if st.excludedAt.IsZero() {
			st.excludedAt = now
			st.outage = t.unreachable(name)
		}
		t.refusals[name] = st
		t.tel.SetDriverCommandFault(name, true, st.reason)
		return
	}
	if !isCommandRefusal(err) {
		return
	}
	if t.refusals == nil {
		t.refusals = map[string]refusalState{}
	}
	st := t.refusals[name]
	if st.consecutive == nil {
		st.consecutive = map[string]int{}
	}
	st.consecutive[action]++
	st.reason = err.Error()
	if st.consecutive[action] >= driverRefusalLimit && st.excludedAt.IsZero() {
		st.excludedAction = action
		st.excludedAt = now
		st.outage = t.unreachable(name)
		t.tel.SetDriverCommandFault(name, true, st.reason)
	}
	t.refusals[name] = st
}

// unreachable reports whether the device is out of reach right now: its
// last poll failed or the watchdog has it offline. A refusal then says the
// wire is down, not that the device turns commands away.
func (t *driverActuationTracker) unreachable(name string) bool {
	h := t.tel.DriverHealth(name)
	return h != nil && (h.Status == telemetry.StatusOffline || h.LastReadFailed())
}

func dispatchAction(payload []byte) string {
	var command struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(payload, &command) != nil {
		return "dispatch"
	}
	if command.Action == "" {
		return "dispatch"
	}
	return command.Action
}

// update walks every driver that cannot actuate to its autonomous default,
// once per transition, and returns the names to send it to. It also ends the
// retry window for drivers excluded long enough to deserve another try, and
// at once for a driver excluded during an outage that reads again; that
// driver gets its default again too.
//
// observeOnly drivers are never returned. A telemetry-only driver must
// receive no command at all, and that includes the safe one: core has no
// mandate over a device it was only asked to watch, and the "default" it
// would send is a write to an inverter somebody else is controlling.
func (t *driverActuationTracker) update(now time.Time, observeOnly map[string]bool) []string {
	if t == nil || t.tel == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	health := t.tel.AllHealth()

	// returned holds drivers back from an outage. Their default went out
	// while the device could not take it, so it is sent again this tick.
	var returned []string
	for name, st := range t.refusals {
		h, known := health[name]
		if !known {
			// Driver removed or restarted: its health record went with
			// it, and so did the fault. Start it over clean.
			delete(t.refusals, name)
			delete(t.defaulted, name)
			continue
		}
		if st.excludedAt.IsZero() {
			continue
		}
		if h.Status == telemetry.StatusOffline && !st.outage {
			st.outage = true
			t.refusals[name] = st
		}
		// Only a watchdog offline marks an outage after the exclusion. A
		// device that answers and refuses keeps the full window even if
		// one of its reads fails meanwhile.
		back := st.outage && h.Status != telemetry.StatusOffline && !h.LastReadFailed()
		if !back && now.Sub(st.excludedAt) < driverRefusalRetryInterval {
			continue
		}
		// Retry window is up, or the device is back. Let one command
		// through: a single fresh refusal puts the driver straight back
		// out, an accepted one clears the record entirely.
		if st.consecutive == nil {
			st.consecutive = map[string]int{}
		}
		st.consecutive[st.excludedAction] = driverRefusalLimit - 1
		st.excludedAt = time.Time{}
		st.outage = false
		t.refusals[name] = st
		t.tel.SetDriverCommandFault(name, false, "")
		// health holds copies, so apply the release to this tick's
		// snapshot too — otherwise the latch below reads the driver as
		// still faulted and defaults a driver we just let back in.
		h.SetCommandFault(false, "")
		health[name] = h
		if back {
			delete(t.defaulted, name)
			returned = append(returned, name)
			slog.Info("excluded driver reads again after an outage — resending its default and letting one command through",
				"driver", name)
			continue
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

	// A driver that also flags its own fault was already added above.
	for _, name := range returned {
		if observeOnly[name] || slices.Contains(pending, name) {
			continue
		}
		pending = append(pending, name)
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
//   - control blocked: handled before this classifier as an authoritative,
//     immediate exclusion because the registry is already holding this driver
//     in its default and retrying with backoff;
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
	case errors.Is(err, drivers.ErrCommandIneligible), errors.Is(err, drivers.ErrCommandSuperseded):
		return false
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return false
	}
	return true
}
