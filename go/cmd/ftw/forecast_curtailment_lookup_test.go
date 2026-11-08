package main

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

// countingCurtailStore wraps the real *state.Store (what main.go passes)
// and counts identity lookups made from the control tick.
type countingCurtailStore struct {
	*state.Store
	lookups atomic.Int64
}

func (c *countingCurtailStore) LookupDeviceByDriverName(name string) *state.Device {
	c.lookups.Add(1)
	return c.Store.LookupDeviceByDriverName(name)
}

func TestPendingDriversSkipsIdentityLookupsWithNothingCurtailed(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	store := &countingCurtailStore{Store: st}
	f := newForecastCurtailment(&stubSender{}, store)
	defer f.Close()
	if f.Active() {
		t.Fatal("precondition: nothing curtailed, nothing persisted")
	}
	names := []string{"meter", "battery", "pv", "ev"}
	for tick := 0; tick < 30; tick++ { // 30 ticks = 1 min at default control_interval_s=2
		if got := f.PendingDrivers(names); len(got) != 0 {
			t.Fatalf("pending=%v", got)
		}
	}
	if n := store.lookups.Load(); n != 0 {
		t.Fatalf("%d identity lookups in 30 ticks with nothing curtailed or persisted", n)
	}
}
