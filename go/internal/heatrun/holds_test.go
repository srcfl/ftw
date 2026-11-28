package heatrun

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/drivercontrol"
	"github.com/srcfl/ftw/go/internal/drivers"
	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

// registry is a driver that records what reaches it.
type registry struct {
	mu       sync.Mutex
	sent     int
	defaults int
}

func (r *registry) SendWithGeneration(context.Context, string, []byte) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent++
	return 1, nil
}

func (r *registry) ControlStatus(string) (drivers.DriverControlStatus, bool) {
	return drivers.DriverControlStatus{Generation: 1}, true
}

func (r *registry) SendDefault(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaults++
	return nil
}

// When the twin stops being trusted while a planned value is held, the
// device gets its own default through the shared holds, and no hold is left
// for an operator to trip over.
func TestUntrustedTwinMidHoldSendsTheDevicesDefault(t *testing.T) {
	reg := &registry{}
	holds := drivercontrol.New(func() drivercontrol.Registry { return reg })
	h := newHouse()
	h.now = time.Date(2026, 11, 17, 15, 0, 0, 0, time.Local)
	h.floor = 22
	s := h.service()
	s.in.Writer = HoldsWriter{Holds: holds}
	defer s.Stop()

	s.Tick(context.Background())
	if reg.sent != 1 || holds.Active("wiser_7") == nil {
		t.Fatalf("setup: sent %d, hold %v (%q)", reg.sent, holds.Active("wiser_7"), s.Snapshot().Zones[0].Write)
	}

	h.status = thermaltwin.StatusLearning
	h.now = h.now.Add(slot)
	s.Tick(context.Background())
	if reg.defaults != 1 {
		t.Fatalf("defaults sent %d, want 1 (%q)", reg.defaults, s.Snapshot().Zones[0].Write)
	}
	if hold := holds.Active("wiser_7"); hold != nil {
		t.Fatalf("hold %+v still armed after the hand-back", hold)
	}
}
