package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type stubCall struct {
	name        string
	payload     string
	hadDeadline bool
}

// stubSender stands in for *drivers.Registry. handler decides how a
// given driver behaves — returning immediately, failing, or hanging the
// way a cloud driver stuck in an unanswered HTTP call does.
type stubSender struct {
	mu      sync.Mutex
	calls   []stubCall
	handler func(ctx context.Context, name string) error
}

func (s *stubSender) Send(ctx context.Context, name string, payload []byte) error {
	_, hadDeadline := ctx.Deadline()
	s.mu.Lock()
	s.calls = append(s.calls, stubCall{name: name, payload: string(payload), hadDeadline: hadDeadline})
	s.mu.Unlock()
	if s.handler == nil {
		return nil
	}
	return s.handler(ctx, name)
}

func (s *stubSender) recorded() []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubCall(nil), s.calls...)
}

// captureWarnings redirects the default logger for one test so the
// timeout warning — the operator's only signal that a driver is slow —
// can be asserted on.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestDriverCommandTimeoutStaysUnderControlInterval(t *testing.T) {
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"one second tick", time.Second, 500 * time.Millisecond},
		{"default two second tick", 2 * time.Second, time.Second},
		{"five second tick caps at the default timeout", 5 * time.Second, driverDefaultTimeout},
		{"slow tick still caps", 30 * time.Second, driverDefaultTimeout},
		{"no interval falls back to the floor", 0, minDriverCommandTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := driverCommandTimeout(tc.interval)
			if got != tc.want {
				t.Fatalf("driverCommandTimeout(%s) = %s, want %s", tc.interval, got, tc.want)
			}
			if got > driverDefaultTimeout {
				t.Errorf("timeout %s exceeds the default-mode timeout %s", got, driverDefaultTimeout)
			}
			if got < minDriverCommandTimeout {
				t.Errorf("timeout %s is below the floor %s", got, minDriverCommandTimeout)
			}
			// The point of the bound: a wedged driver must cost its
			// own command, not the tick it rides in.
			if tc.interval >= minDriverCommandTimeout*2 && got >= tc.interval {
				t.Errorf("timeout %s does not fit inside the %s control interval", got, tc.interval)
			}
		})
	}
}

// The bug this fix closes: dispatch handed Registry.Send the
// process-lifetime context, which has no deadline, so a driver wedged in
// its device I/O held the control tick for the whole site.
func TestSendDriverCommandStopsWaitingOnWedgedDriver(t *testing.T) {
	logs := captureWarnings(t)
	const timeout = 50 * time.Millisecond
	wedged := make(chan struct{})
	defer close(wedged)
	sender := &stubSender{handler: func(ctx context.Context, name string) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wedged:
			return nil
		}
	}}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		// context.Background() stands in for the process-lifetime
		// context the control loop carries.
		sendDriverCommand(context.Background(), sender, "driver send", "cloud-battery", []byte(`{"action":"battery","power_w":-2000}`), timeout)
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		if elapsed < timeout {
			t.Fatalf("returned after %s, before the %s deadline", elapsed, timeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sendDriverCommand never returned: a wedged driver is holding the control tick")
	}

	if got := logs.String(); !strings.Contains(got, "driver send timed out") || !strings.Contains(got, "cloud-battery") {
		t.Errorf("timeout log does not name the slow driver: %q", got)
	}
}

// One wedged driver must cost its own command, not the batteries queued
// behind it — the fuse guard dispatches through this same loop.
func TestWedgedDriverDoesNotStarveLaterDispatchTargets(t *testing.T) {
	captureWarnings(t)
	const timeout = 50 * time.Millisecond
	wedged := make(chan struct{})
	defer close(wedged)
	sender := &stubSender{handler: func(ctx context.Context, name string) error {
		if name != "cloud-battery" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wedged:
			return nil
		}
	}}

	targets := []string{"cloud-battery", "ferroamp", "sungrow"}
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for _, name := range targets {
			sendDriverCommand(context.Background(), sender, "driver send", name, []byte(`{"action":"battery","power_w":0}`), timeout)
		}
		done <- time.Since(start)
	}()

	var elapsed time.Duration
	select {
	case elapsed = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch never finished: the wedged driver blocked the drivers behind it")
	}
	// Only the wedged driver may spend its full deadline.
	if elapsed > 2*timeout {
		t.Errorf("dispatch of %d drivers took %s, want roughly one %s timeout", len(targets), elapsed, timeout)
	}
	calls := sender.recorded()
	if len(calls) != len(targets) {
		t.Fatalf("sent %d commands, want %d", len(calls), len(targets))
	}
	for i, call := range calls {
		if call.name != targets[i] {
			t.Errorf("command %d went to %q, want %q", i, call.name, targets[i])
		}
	}
}

func TestSendDriverCommandLeavesHealthyDriverUntouched(t *testing.T) {
	logs := captureWarnings(t)
	sender := &stubSender{}
	payload := []byte(`{"action":"curtail","power_w":3000}`)

	start := time.Now()
	sendDriverCommand(context.Background(), sender, "pv curtail send", "solaredge", payload, time.Second)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("a healthy command took %s", elapsed)
	}

	calls := sender.recorded()
	if len(calls) != 1 {
		t.Fatalf("sent %d commands, want 1", len(calls))
	}
	if calls[0].name != "solaredge" || calls[0].payload != string(payload) {
		t.Errorf("command = %+v, want the curtail payload for solaredge", calls[0])
	}
	if !calls[0].hadDeadline {
		t.Error("driver received a context with no deadline")
	}
	if got := logs.String(); got != "" {
		t.Errorf("healthy command logged %q, want silence", got)
	}
}

// A driver that refuses a command still reports as before: the deadline
// must not swallow real driver errors.
func TestSendDriverCommandLogsDriverError(t *testing.T) {
	logs := captureWarnings(t)
	sender := &stubSender{handler: func(ctx context.Context, name string) error {
		return errors.New("modbus write refused")
	}}

	sendDriverCommand(context.Background(), sender, "driver send", "ferroamp", []byte(`{"action":"battery","power_w":0}`), time.Second)

	got := logs.String()
	if !strings.Contains(got, "modbus write refused") || !strings.Contains(got, "ferroamp") {
		t.Errorf("driver error not logged: %q", got)
	}
	if strings.Contains(got, "timed out") {
		t.Errorf("a refusal was reported as a timeout: %q", got)
	}
}
