package main

import (
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/ha"
	"github.com/srcfl/ftw/go/internal/state"
)

// A box with no stored history yet gives Home Assistant lifetime totals of
// zero at once; without a store there is no energy source at all.
func TestHAEnergySourceLifetimeTotals(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, ok := haEnergySource(st).LifetimeEnergy()
	if !ok || got != (ha.EnergyTotals{}) {
		t.Fatalf("LifetimeEnergy() = %+v, %v; want zero totals, known", got, ok)
	}
	if haEnergySource(nil) != nil {
		t.Fatal("a nil store must leave the energy sensors out")
	}
}
