package main

import (
	"fmt"
	"testing"
)

// An active entry that the background worker moves into f.ids while
// PendingDrivers runs must still be reported as pending. The long name list
// widens the window; the old unlock gap missed pv in most runs.
func TestPendingDriversKeepsEntryMovedByPersist(t *testing.T) {
	names := make([]string, 0, 20001)
	for i := 0; i < 20000; i++ {
		names = append(names, fmt.Sprintf("other-%d", i))
	}
	names = append(names, "pv")
	misses := 0
	const iters = 100
	for i := 0; i < iters; i++ {
		store := &curtailStore{identities: map[string]string{"pv": "hardware-1"}}
		f := newForecastCurtailment(&stubSender{}, store)
		f.record("pv", true) // wakes the worker, which moves entry -> ids
		got := f.PendingDrivers(names)
		if len(got) != 1 || got[0] != "pv" {
			misses++
		}
		f.Close()
	}
	if misses > 0 {
		t.Fatalf("pv curtailed but not pending in %d/%d runs", misses, iters)
	}
}
