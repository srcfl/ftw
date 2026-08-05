package main

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// driverCommandSender is the part of *drivers.Registry the dispatch
// sends use. Narrow, so the deadline behaviour can be exercised without
// a live driver host.
type driverCommandSender interface {
	Send(ctx context.Context, name string, payload []byte) error
}

// minDriverCommandTimeout is the floor under driverCommandTimeout. A
// deadline shorter than a LAN Modbus write would fail every command and
// leave the site frozen at its last output, which is worse than the
// stall the deadline exists to prevent.
const minDriverCommandTimeout = 250 * time.Millisecond

// driverCommandTimeout bounds one dispatch command, derived from the
// operator's control interval.
//
// The hazard: Registry.Send blocks until the driver goroutine has run
// driver_command, and that goroutine performs the device I/O inline. A
// cloud driver waiting on an HTTP or OAuth call that never answers
// therefore holds the whole control tick — every other battery on the
// site, and the reactive fuse guard that protects the main breaker with
// them. The context the tick carries is the process-lifetime one, which
// has no deadline, so nothing ever breaks that wait. sendDriverDefault
// already bounds its call for exactly this reason, and SendDefault's own
// doc comment names the deadlock; the dispatch path runs every control
// interval and needs the same bound.
//
// Half the interval: a wedged driver then costs its own command and
// still leaves the rest of the tick — the remaining drivers, the fuse
// guard, the state save — inside the interval. Capped at
// driverDefaultTimeout because a dispatch command must never be given
// longer to give up than the safety default that has to reach the same
// driver afterwards.
func driverCommandTimeout(controlInterval time.Duration) time.Duration {
	timeout := controlInterval / 2
	if timeout > driverDefaultTimeout {
		timeout = driverDefaultTimeout
	}
	if timeout < minDriverCommandTimeout {
		timeout = minDriverCommandTimeout
	}
	return timeout
}

// sendDriverCommand sends one dispatch command under its own deadline and
// returns what the driver made of it. A timeout is logged at Warn with the
// driver name so a chronically slow driver shows up in the log instead of
// quietly eating tick cadence; kind names the dispatch path ("driver send",
// "pv curtail send").
//
// A timeout is deliberately not recorded as a driver failure. The driver
// goroutine serialises polls and commands, so a driver wedged inside
// driver_command stops emitting telemetry as well, and the staleness
// watchdog already walks it to its autonomous default mode. Counting the
// timeout separately would double-book the same fault and could push a
// merely slow cloud driver out of control on one bad round trip.
// driverActuationTracker applies the same rule to the error it returns.
func sendDriverCommand(ctx context.Context, reg driverCommandSender, kind, name string, payload []byte, timeout time.Duration) error {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := reg.Send(cmdCtx, name, payload)
	switch {
	case err == nil:
	case errors.Is(err, context.DeadlineExceeded):
		slog.Warn(kind+" timed out", "name", name, "timeout", timeout)
	default:
		slog.Warn(kind, "name", name, "err", err)
	}
	return err
}
