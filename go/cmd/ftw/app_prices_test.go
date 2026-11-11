package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/prices"
	"github.com/srcfl/ftw/go/internal/state"
)

// appPrices holds the only arithmetic on the box side of the price path: a
// lookback wide enough to catch the slot already running, and an upper bound
// pulled back by a millisecond because the store's BETWEEN is inclusive at
// both ends. appproto's fake reimplements the overlap rule rather than calling
// this, so without a test here the two can drift and nothing would say so.
//
// Against a real store, the way energy_history_test.go does — the inclusive
// BETWEEN is SQLite's, and a fake would just agree with whatever this file
// assumed.

func priceRig(t *testing.T) (*appPrices, *state.Store) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &appPrices{svc: &prices.Service{Store: st, Zone: "SE3", Currency: "SEK"}}, st
}

func savePrices(t *testing.T, st *state.Store, zone string, startMs int64, lenMin, n int) {
	t.Helper()
	step := int64(lenMin) * 60_000
	pts := make([]state.PricePoint, 0, n)
	for i := range n {
		pts = append(pts, state.PricePoint{
			Zone:        zone,
			SlotTsMs:    startMs + int64(i)*step,
			SlotLenMin:  lenMin,
			SpotOreKwh:  float64(40 + i),
			TotalOreKwh: float64(90 + i),
		})
	}
	if err := st.SavePrices(pts); err != nil {
		t.Fatal(err)
	}
}

func TestAppPricesServesTheSlotAlreadyRunningAtTheStartOfTheWindow(t *testing.T) {
	// A window starting at "now" opens in the middle of a slot. Leaving that
	// slot out would leave the app unable to say what the house is paying
	// right this minute, which is the first thing the price view says.
	a, st := priceRig(t)
	at := int64(1_760_000_000_000)
	savePrices(t, st, "SE3", at, 60, 6)

	got, err := a.Slots(context.Background(), at+30*60_000, at+90*60_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d slots, want the one in progress and the one after it: %+v", len(got), got)
	}
	if got[0].StartMs != at {
		t.Errorf("first slot starts at %d, want %d — the slot the window opened inside", got[0].StartMs, at)
	}
}

func TestAppPricesEndsTheWindowHalfOpen(t *testing.T) {
	// The store selects on slot start with BETWEEN, inclusive at both ends, so
	// without the -1 a slot beginning exactly where the window closes would be
	// served in both this window and the next one.
	a, st := priceRig(t)
	at := int64(1_760_000_000_000)
	savePrices(t, st, "SE3", at, 60, 4)

	got, err := a.Slots(context.Background(), at, at+2*3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d slots for a two-hour window of hourly prices: %+v", len(got), got)
	}
	if last := got[len(got)-1]; last.StartMs != at+3_600_000 {
		t.Errorf("last slot starts at %d, want %d", last.StartMs, at+3_600_000)
	}
}

func TestAppPricesDropsWhatTheLookbackOvershot(t *testing.T) {
	// The lookback is two hours because that is the longest slot any provider
	// publishes. On a quarter-hour market it reaches back seven slots too far,
	// and every one of them ended before the window opened.
	a, st := priceRig(t)
	at := int64(1_760_000_000_000)
	savePrices(t, st, "SE3", at, 15, 16)

	from := at + 2*3_600_000 // four hours of quarter-hours in, nothing overlaps
	got, err := a.Slots(context.Background(), from, from+3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		end := p.StartMs + int64(p.LenMin)*60_000
		if end <= from {
			t.Fatalf("slot ending at %d was served for a window opening at %d", end, from)
		}
	}
}

// The zero-length guard in Slots has no test: SavePrices rewrites any length
// at or below zero to sixty on the way in, so no row that reaches it can trip
// the guard. A test would pass with the guard removed, which is worse than
// none.

func TestAppPricesServesOnlyTheConfiguredZone(t *testing.T) {
	a, st := priceRig(t)
	at := int64(1_760_000_000_000)
	savePrices(t, st, "SE3", at, 60, 2)
	savePrices(t, st, "NO2", at, 60, 2)

	got, err := a.Slots(context.Background(), at, at+2*3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d slots, want only SE3's two: %+v", len(got), got)
	}
	if a.Zone() != "SE3" || a.Currency() != "SEK" {
		t.Errorf("zone/currency = %q/%q", a.Zone(), a.Currency())
	}
}

// The lookback has to be at least as wide as the longest slot the box will
// ever store, or a window opening inside a long slot misses it.
func TestTheLookbackCoversTheLongestSlot(t *testing.T) {
	a, st := priceRig(t)
	at := int64(1_760_000_000_000)
	savePrices(t, st, "SE3", at, maxPriceSlotMin, 1)

	from := at + (maxPriceSlotMin-1)*60_000 // one minute before it ends
	got, err := a.Slots(context.Background(), from, from+time.Hour.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].StartMs != at {
		t.Fatalf("the slot in progress was missed: %+v", got)
	}
}

// The app and FTW Cloud get the rate the service taxes at, after its default.
// A box that never set vat_percent taxes at 25 % and must say so, not send
// the config's 0 as "no VAT".
func TestAppPricesSendsTheServicesVATRate(t *testing.T) {
	svc := prices.FromConfig(&config.Price{Provider: "sourceful", Zone: "SE3"}, nil, nil)
	a := &appPrices{svc: svc}
	if got := a.VATPercent(); got != 25 {
		t.Fatalf("unset VAT went out as %v, want the default 25", got)
	}
	svc.SetApplier(prices.Applier{VATPercent: 25.5})
	if got := a.VATPercent(); got != 25.5 {
		t.Fatalf("after a reload VAT went out as %v, want 25.5", got)
	}
}
