package state

import (
	"path/filepath"
	"testing"
)

// A day cached at one resolution and fetched again at another must not keep
// both: the planner rejects a timeline whose slots overlap.
func TestSavePricesReplacesOverlappingRows(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const hour = int64(3_600_000)
	const quarter = hour / 4
	base := int64(1_800_000_000_000) // on the hour
	row := func(zone string, start int64, lenMin int, source string) PricePoint {
		return PricePoint{Zone: zone, SlotTsMs: start, SlotLenMin: lenMin,
			SpotOreKwh: 50, TotalOreKwh: 60, Source: source, FetchedAtMs: 1}
	}
	var quarters []PricePoint
	for i := int64(0); i < 8; i++ {
		quarters = append(quarters, row("SE3", base+i*quarter, 15, "quarters"))
	}
	// Another zone at the same times and the SE3 hour after are untouched.
	quarters = append(quarters, row("SE4", base+quarter, 15, "other-zone"))
	if err := st.SavePrices(quarters); err != nil {
		t.Fatal(err)
	}
	// The first hour comes back hourly; the second hour stays quarterly.
	if err := st.SavePrices([]PricePoint{row("SE3", base, 60, "hourly")}); err != nil {
		t.Fatal(err)
	}
	assertTimeline(t, st, "SE3", base, 2*hour, []int64{base, base + 4*quarter, base + 5*quarter, base + 6*quarter, base + 7*quarter}, []int{60, 15, 15, 15, 15})
	assertTimeline(t, st, "SE4", base, 2*hour, []int64{base + quarter}, []int{15})

	// Back to quarters, including one that starts inside the cached hour.
	if err := st.SavePrices([]PricePoint{row("SE3", base+2*quarter, 15, "quarters")}); err != nil {
		t.Fatal(err)
	}
	assertTimeline(t, st, "SE3", base, 2*hour, []int64{base + 2*quarter, base + 4*quarter, base + 5*quarter, base + 6*quarter, base + 7*quarter}, []int{15, 15, 15, 15, 15})

	// One batch that carries both resolutions still stores no overlap.
	if err := st.SavePrices([]PricePoint{
		row("SE3", base+4*quarter, 15, "quarters"),
		row("SE3", base+4*quarter, 60, "hourly"),
		row("SE3", base+5*quarter, 15, "quarters"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadPrices("SE3", base, base+2*hour)
	if err != nil {
		t.Fatal(err)
	}
	var endMs int64
	for i, p := range got {
		if i > 0 && p.SlotTsMs < endMs {
			t.Fatalf("row %d overlaps the previous one: %+v", i, got)
		}
		endMs = p.SlotTsMs + int64(p.SlotLenMin)*60_000
	}
}

func assertTimeline(t *testing.T, st *Store, zone string, since, span int64, starts []int64, lens []int) {
	t.Helper()
	got, err := st.LoadPrices(zone, since, since+span)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(starts) {
		t.Fatalf("%s rows=%+v, want starts %v", zone, got, starts)
	}
	for i, p := range got {
		if p.SlotTsMs != starts[i] || p.SlotLenMin != lens[i] {
			t.Fatalf("%s rows=%+v, want starts %v lengths %v", zone, got, starts, lens)
		}
	}
}
