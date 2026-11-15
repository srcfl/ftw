package drivercontrol

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/drivers"
)

type fakeRegistry struct {
	mu         sync.Mutex
	generation uint64
	running    bool
	sendErr    error
	sent       int
	defaults   int
}

func (f *fakeRegistry) SendWithGeneration(_ context.Context, _ string, _ []byte) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return 0, f.sendErr
	}
	f.sent++
	return f.generation, nil
}

func (f *fakeRegistry) ControlStatus(string) (drivers.DriverControlStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return drivers.DriverControlStatus{Generation: f.generation}, f.running
}

func (f *fakeRegistry) SendDefault(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaults++
	return nil
}

func (f *fakeRegistry) counts() (sent, defaults int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent, f.defaults
}

func newHolds(reg *fakeRegistry) *Holds {
	return New(func() Registry { return reg })
}

func send(t *testing.T, h *Holds, value any, d time.Duration) (Hold, error) {
	t.Helper()
	var hold Hold
	var err error
	h.Send(context.Background(), "heat", "set_offset", value, []byte(`{}`), d, nil, func(got Hold, e error) {
		hold, err = got, e
	})
	return hold, err
}

func TestSendArmsAHoldBoundToTheGeneration(t *testing.T) {
	reg := &fakeRegistry{generation: 7, running: true}
	h := newHolds(reg)
	hold, err := send(t, h, 2.0, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if hold.Generation != 7 || hold.Control != "set_offset" || hold.Value != 2.0 {
		t.Fatalf("hold = %+v", hold)
	}
	if got := h.Active("heat"); got == nil || got.Generation != 7 {
		t.Fatalf("Active = %+v", got)
	}
}

func TestFailedSendClearsTheOldHold(t *testing.T) {
	reg := &fakeRegistry{generation: 1, running: true}
	h := newHolds(reg)
	if _, err := send(t, h, 1.0, time.Hour); err != nil {
		t.Fatal(err)
	}
	reg.mu.Lock()
	reg.sendErr = errors.New("device gone")
	reg.mu.Unlock()
	if _, err := send(t, h, 2.0, time.Hour); err == nil {
		t.Fatal("send error hidden")
	}
	if got := h.Active("heat"); got != nil {
		t.Fatalf("failed send left hold %+v", got)
	}
}

func TestExpirySendsTheDefault(t *testing.T) {
	reg := &fakeRegistry{generation: 1, running: true}
	h := newHolds(reg)
	if _, err := send(t, h, 1.0, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, d := reg.counts(); d == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, d := reg.counts(); d != 1 {
		t.Fatalf("defaults after expiry = %d, want 1", d)
	}
	if got := h.Active("heat"); got != nil {
		t.Fatalf("hold survived expiry: %+v", got)
	}
}

// A stopped generation must never default its replacement.
func TestExpiryOfAnOldGenerationSendsNoDefault(t *testing.T) {
	reg := &fakeRegistry{generation: 1, running: true}
	h := newHolds(reg)
	if _, err := send(t, h, 1.0, time.Hour); err != nil {
		t.Fatal(err)
	}
	state := h.Peek("heat")
	fired := state.CurrentHold()
	reg.mu.Lock()
	reg.generation = 2
	reg.mu.Unlock()
	if err := h.Expire("heat", state, fired); err != nil {
		t.Fatal(err)
	}
	if _, d := reg.counts(); d != 0 {
		t.Fatalf("old generation sent %d defaults", d)
	}
}

// A replaced hold's timer must not default the new one.
func TestExpiryOfAReplacedHoldIsANoOp(t *testing.T) {
	reg := &fakeRegistry{generation: 1, running: true}
	h := newHolds(reg)
	if _, err := send(t, h, 1.0, time.Hour); err != nil {
		t.Fatal(err)
	}
	state := h.Peek("heat")
	old := state.CurrentHold()
	if _, err := send(t, h, 2.0, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := h.Expire("heat", state, old); err != nil {
		t.Fatal(err)
	}
	if _, d := reg.counts(); d != 0 {
		t.Fatalf("replaced hold sent %d defaults", d)
	}
	if got := h.Active("heat"); got == nil || got.Value != 2.0 {
		t.Fatalf("Active = %+v, want the replacement", got)
	}
}

func TestSendDefaultClearsTheHold(t *testing.T) {
	reg := &fakeRegistry{generation: 1, running: true}
	h := newHolds(reg)
	if _, err := send(t, h, 1.0, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := h.SendDefault(context.Background(), "heat", nil); err != nil {
		t.Fatal(err)
	}
	if _, d := reg.counts(); d != 1 {
		t.Fatalf("defaults = %d, want 1", d)
	}
	if got := h.Active("heat"); got != nil {
		t.Fatalf("hold survived default: %+v", got)
	}
}

func TestSendDefaultForAnUnknownDriverFails(t *testing.T) {
	h := newHolds(&fakeRegistry{})
	if err := h.SendDefault(context.Background(), "heat", nil); err == nil {
		t.Fatal("default to a driver that is not running succeeded")
	}
}

func TestClearDropsStateAndTimer(t *testing.T) {
	reg := &fakeRegistry{generation: 1, running: true}
	h := newHolds(reg)
	if _, err := send(t, h, 1.0, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	h.Clear("heat")
	if h.Len() != 0 || h.Peek("heat") != nil {
		t.Fatalf("Clear left state: len=%d", h.Len())
	}
	time.Sleep(60 * time.Millisecond)
	if _, d := reg.counts(); d != 0 {
		t.Fatalf("cleared hold's timer sent %d defaults", d)
	}
}

// Without a registry nothing is sent and nothing is held.
func TestNoRegistry(t *testing.T) {
	h := New(func() Registry { return nil })
	if _, err := send(t, h, 1.0, time.Hour); err == nil {
		t.Fatal("send without a registry succeeded")
	}
	if err := h.SendDefault(context.Background(), "heat", nil); err == nil {
		t.Fatal("default without a registry succeeded")
	}
	if h.Active("heat") != nil {
		t.Fatal("hold without a registry")
	}
}
