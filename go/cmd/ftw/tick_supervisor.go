package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// The control tick runs every safety check itself: the driver watchdog, the
// stale-meter stop and the fuse guard. A tick that wedges therefore leaves
// devices on their last forced setpoint, and a Sungrow leaves forced mode only
// through its default. tickSupervisor watches the tick from outside.
//
// Once the last completed tick is older than tickStallAfter, it sends every
// driver its default mode. Once it is older than tickExitAfter, it exits the
// process so the service manager restarts Core; startup sends defaults again,
// and a native probation counts the exit as a crash.
//
// Ages use the monotonic clock. A Pi without an RTC boots on a stale clock and
// NTP steps it forward, often while the loop already runs; a wall-clock age
// would then trip a healthy Core, and a backward step would hide a stall.
type tickSupervisor struct {
	// base carries a monotonic reading when it comes from time.Now.
	// lastNs is the time from base to the last completed tick.
	base       time.Time
	lastNs     atomic.Int64
	intervalNs atomic.Int64

	// stalled is touched only by the supervisor goroutine.
	stalled bool
}

const tickStallFloor = 10 * time.Second
const tickExitFloor = 60 * time.Second

func newTickSupervisor(now time.Time, interval time.Duration) *tickSupervisor {
	s := &tickSupervisor{base: now}
	s.beat(now, interval)
	return s
}

// beat records a completed tick and the control interval it ran at.
func (s *tickSupervisor) beat(now time.Time, interval time.Duration) {
	s.intervalNs.Store(int64(interval))
	s.lastNs.Store(int64(now.Sub(s.base)))
}

func tickStallAfter(interval time.Duration) time.Duration {
	return max(3*interval, tickStallFloor)
}

func tickExitAfter(interval time.Duration) time.Duration {
	return max(tickExitFloor, 2*tickStallAfter(interval))
}

// check reports what the supervisor must do at now. sendDefaults is true once
// per stall; exit stays true while the stall lasts past tickExitAfter.
func (s *tickSupervisor) check(now time.Time) (sendDefaults, exit bool, age time.Duration) {
	interval := time.Duration(s.intervalNs.Load())
	age = now.Sub(s.base) - time.Duration(s.lastNs.Load())
	if age < tickStallAfter(interval) {
		if s.stalled {
			slog.Info("control tick recovered", "age", age)
		}
		s.stalled = false
		return false, false, age
	}
	sendDefaults = !s.stalled
	s.stalled = true
	return sendDefaults, age >= tickExitAfter(interval), age
}

// run checks the tick every period until ctx ends. sendDefaults runs in its
// own goroutine, so a default that blocks cannot hold back the exit.
func (s *tickSupervisor) run(ctx context.Context, period time.Duration, now func() time.Time, sendDefaults func(), exit func()) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		send, stop, age := s.check(now())
		if send {
			slog.Error("control tick stalled — reverting every driver to autonomous", "age", age)
			go sendDefaults()
		}
		if stop {
			slog.Error("control tick still stalled — exiting so the service restarts Core", "age", age)
			exit()
			return
		}
	}
}

// sendDefaultsInParallel sends every named driver its default at once, so one
// slow driver does not hold back the others.
func sendDefaultsInParallel(names []string, send func(name string)) {
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			send(name)
		}()
	}
	wg.Wait()
}

// withCtrlLock runs fn under ctrlMu with a deferred unlock. A panic in
// dispatch math then releases the lock, and the process exits through its
// deferred shutdown, which sends driver defaults. Without the defer, shutdown
// can hang on a replan or the HA bridge waiting for ctrlMu.
func withCtrlLock(mu *sync.Mutex, fn func()) {
	mu.Lock()
	defer mu.Unlock()
	fn()
}
