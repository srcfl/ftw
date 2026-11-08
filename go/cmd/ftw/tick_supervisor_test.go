package main

import (
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A tick that blocks after it has dispatched leaves the battery on its last
// setpoint. The supervisor must send defaults once the tick is 10 s old on a
// 2 s interval, and exit at 60 s so the service restarts Core.
func TestTickSupervisorDefaultsThenExitsOnBlockedTick(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }

	sup := newTickSupervisor(t0, 2*time.Second)

	// The control loop: one tick completes, the next blocks for good.
	block := make(chan struct{})
	defer close(block)
	ticked := make(chan struct{})
	go func() {
		sup.beat(now(), 2*time.Second)
		close(ticked)
		<-block
	}()
	<-ticked

	defaults := make(chan struct{}, 4)
	exited := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.run(ctx, time.Millisecond, now,
		func() { defaults <- struct{}{} },
		func() { close(exited) })

	clock.Store(t0.Add(9 * time.Second).UnixNano())
	expectNone(t, defaults, "defaults before the tick is 10 s old")

	clock.Store(t0.Add(11 * time.Second).UnixNano())
	expectOne(t, defaults, "defaults once the tick is 10 s old")
	clock.Store(t0.Add(30 * time.Second).UnixNano())
	expectNone(t, defaults, "a second default in the same stall")
	select {
	case <-exited:
		t.Fatal("exited before 60 s")
	default:
	}

	clock.Store(t0.Add(61 * time.Second).UnixNano())
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("no exit after the tick was 61 s old")
	}
}

func TestTickSupervisorRearmsAfterTheTickRecovers(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	sup := newTickSupervisor(t0, 2*time.Second)

	if send, exit, _ := sup.check(t0.Add(12 * time.Second)); !send || exit {
		t.Fatalf("first stall: send=%v exit=%v, want defaults only", send, exit)
	}
	sup.beat(t0.Add(13*time.Second), 2*time.Second)
	if send, exit, _ := sup.check(t0.Add(14 * time.Second)); send || exit {
		t.Fatalf("healthy tick: send=%v exit=%v", send, exit)
	}
	if send, _, _ := sup.check(t0.Add(24 * time.Second)); !send {
		t.Fatal("a new stall after recovery sent no defaults")
	}
}

func TestTickSupervisorThresholdsFollowTheInterval(t *testing.T) {
	cases := []struct {
		interval, stall, exit time.Duration
	}{
		{2 * time.Second, 10 * time.Second, 60 * time.Second},
		{5 * time.Second, 15 * time.Second, 60 * time.Second},
		{30 * time.Second, 90 * time.Second, 180 * time.Second},
	}
	for _, c := range cases {
		if got := tickStallAfter(c.interval); got != c.stall {
			t.Errorf("interval %v: stall after %v, want %v", c.interval, got, c.stall)
		}
		if got := tickExitAfter(c.interval); got != c.exit {
			t.Errorf("interval %v: exit after %v, want %v", c.interval, got, c.exit)
		}
	}
}

func TestSendDefaultsInParallelDoesNotWaitOnOneSlowDriver(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var sent []string
	done := make(chan struct{})
	go func() {
		sendDefaultsInParallel([]string{"slow", "sungrow", "meter"}, func(name string) {
			if name == "slow" {
				<-release
			}
			mu.Lock()
			sent = append(sent, name)
			mu.Unlock()
		})
		close(done)
	}()
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(sent)
		mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("defaults sent while one driver blocks: %d, want 2", n)
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	<-done
}

func TestWithCtrlLockReleasesTheLockOnPanic(t *testing.T) {
	var mu sync.Mutex
	func() {
		defer func() { _ = recover() }()
		withCtrlLock(&mu, func() { panic("dispatch math") })
	}()
	if !mu.TryLock() {
		t.Fatal("ctrlMu still held after a panic in dispatch")
	}
}

// The native trial commits from the first completed control tick, and every
// path out of a tick records it, so the supervisor never trips on a healthy
// stale-meter or setup-measurement tick.
func TestNativeTrialCommitsOnlyAfterFirstControlTick(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if n := strings.Count(s, "trial.complete(selfUpdater)"); n != 1 {
		t.Fatalf("trial.complete appears %d times, want 1", n)
	}
	commit := strings.Index(s, "trial.complete(selfUpdater)")
	boot := strings.Index(s, "close(bootDone)")
	done := strings.Index(s, "tickDone := func() {")
	tick := strings.Index(s, "case <-ticker.C:\n\t\t\tcfgMu.RLock()")
	if boot < 0 || done < 0 || tick < 0 {
		t.Fatalf("markers not found: boot=%d tickDone=%d tick=%d", boot, done, tick)
	}
	if commit < boot || commit < done || commit > tick {
		t.Errorf("trial commits at byte %d, outside tickDone (%d..%d)", commit, done, tick)
	}

	body := s[tick:]
	body = body[:strings.Index(body, "\nfunc ")]
	exits := strings.Count(body, "\n\t\t\t\tcontinue\n")
	beats := strings.Count(body, "tickDone()\n\t\t\t\tcontinue\n")
	if exits == 0 || beats != exits {
		t.Errorf("%d tick exits by continue, %d of them call tickDone", exits, beats)
	}
	if !strings.HasSuffix(body, "\t\t\ttickDone()\n\t\t}\n\t}\n}\n") {
		t.Error("the tick does not end with tickDone()")
	}
}

func expectOne(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("missing: %s", what)
	}
}

func expectNone(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("unexpected: %s", what)
	case <-time.After(50 * time.Millisecond):
	}
}
