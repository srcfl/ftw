package main

import (
	"testing"
	"time"
	"unsafe"
)

// stepWall returns t with its wall clock moved by step and its monotonic
// reading kept, which is how time.Now looks after NTP steps the clock. It
// edits time.Time's private layout, so it checks the result and fails loudly
// if a Go release changes that layout.
func stepWall(tb testing.TB, t time.Time, step time.Duration) time.Time {
	tb.Helper()
	if step%time.Second != 0 {
		tb.Fatal("stepWall takes whole seconds")
	}
	const hasMonotonic = 1 << 63
	const nsecShift = 30
	stepped := t
	raw := (*struct {
		wall uint64
		ext  int64
		loc  *time.Location
	})(unsafe.Pointer(&stepped))
	if raw.wall&hasMonotonic == 0 {
		tb.Fatal("stepWall needs a time from time.Now")
	}
	raw.wall = uint64(int64(raw.wall) + int64(step/time.Second)<<nsecShift)
	if got := stepped.Round(0).Sub(t.Round(0)); got != step {
		tb.Fatalf("time.Time layout changed: wall moved %v, want %v", got, step)
	}
	if got := stepped.Sub(t); got != 0 {
		tb.Fatalf("time.Time layout changed: monotonic moved %v", got)
	}
	return stepped
}

// A Pi without an RTC boots on a stale clock and NTP steps it forward while
// the loop runs. A healthy tick must not trip the supervisor.
func TestTickSupervisorIgnoresWallClockStep(t *testing.T) {
	t0 := time.Now()
	sup := newTickSupervisor(t0, 2*time.Second)
	now := stepWall(t, t0.Add(2*time.Second), 10*time.Minute)
	if send, exit, age := sup.check(now); send || exit {
		t.Fatalf("healthy tick after an NTP step: send=%v exit=%v age=%v", send, exit, age)
	}

	// Ticks recorded after the step keep the supervisor quiet as well.
	sup.beat(now, 2*time.Second)
	if send, exit, age := sup.check(now.Add(time.Second)); send || exit {
		t.Fatalf("tick after the step: send=%v exit=%v age=%v", send, exit, age)
	}
}

// A backward step must not hide a real stall.
func TestTickSupervisorSeesStallAfterBackwardStep(t *testing.T) {
	t0 := time.Now()
	sup := newTickSupervisor(t0, 2*time.Second)
	now := stepWall(t, t0.Add(70*time.Second), -time.Hour)
	if send, exit, age := sup.check(now); !send || !exit {
		t.Fatalf("70 s stall after a backward step: send=%v exit=%v age=%v", send, exit, age)
	}
}
