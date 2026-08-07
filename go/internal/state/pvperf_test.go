package state

import (
	"testing"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func TestSaveIrradianceRoundtrip(t *testing.T) {
	s := freshStore(t)
	rows := []IrradianceRow{
		{SlotTsMs: 1000, GHIWm2: 300, DHIWm2: f64(80), Source: "strang", FetchedAtMs: 5000},
		{SlotTsMs: 2000, GHIWm2: 450, DHIWm2: nil, Source: "strang", FetchedAtMs: 5000},
	}
	if err := s.SaveIrradiance(rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadIrradiance(0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].SlotTsMs != 1000 || got[0].GHIWm2 != 300 || got[0].DHIWm2 == nil || *got[0].DHIWm2 != 80 {
		t.Errorf("row0 mismatch: %+v", got[0])
	}
	if got[1].DHIWm2 != nil {
		t.Errorf("row1 diffuse should be nil, got %v", *got[1].DHIWm2)
	}

	// Upsert overwrites, no duplicate.
	if err := s.SaveIrradiance([]IrradianceRow{{SlotTsMs: 1000, GHIWm2: 999, Source: "strang", FetchedAtMs: 6000}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.LoadIrradiance(0, 10000)
	if len(got) != 2 {
		t.Fatalf("upsert should not add a row, got %d", len(got))
	}
	if got[0].GHIWm2 != 999 {
		t.Errorf("upsert should overwrite ghi, got %.0f", got[0].GHIWm2)
	}
}

func TestLoadIrradianceRangeFilters(t *testing.T) {
	s := freshStore(t)
	_ = s.SaveIrradiance([]IrradianceRow{
		{SlotTsMs: 100, GHIWm2: 1, Source: "strang", FetchedAtMs: 1},
		{SlotTsMs: 200, GHIWm2: 2, Source: "strang", FetchedAtMs: 1},
		{SlotTsMs: 300, GHIWm2: 3, Source: "strang", FetchedAtMs: 1},
	})
	got, err := s.LoadIrradiance(150, 250)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SlotTsMs != 200 {
		t.Fatalf("range filter failed: %+v", got)
	}
}

func TestPVPerformanceRoundtrip(t *testing.T) {
	s := freshStore(t)
	day := "2026-06-21"
	p := PVPerformanceDay{
		Day:              day,
		ExpectedWh:       12000,
		ActualWh:         10800,
		PR:               f64(0.9),
		StrangDataDateMs: i64(1719000000000),
	}
	if err := s.SavePVPerformance(p); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.LoadPVPerformanceDay(day)
	if err != nil || !ok {
		t.Fatalf("load ok=%v err=%v", ok, err)
	}
	if got.ExpectedWh != 12000 || got.ActualWh != 10800 || got.PR == nil || *got.PR != 0.9 {
		t.Errorf("mismatch: %+v", got)
	}
	if got.StrangDataDateMs == nil || *got.StrangDataDateMs != 1719000000000 {
		t.Errorf("provenance mismatch: %+v", got.StrangDataDateMs)
	}
	if got.ComputedAtMs == 0 {
		t.Error("computed_at_ms should be stamped")
	}

	// Upsert with n/a PR (nil) overwrites.
	p.PR = nil
	p.ExpectedWh = 50
	if err := s.SavePVPerformance(p); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.LoadPVPerformanceDay(day)
	if got.PR != nil {
		t.Errorf("PR should be nil after upsert, got %v", *got.PR)
	}
	if got.ExpectedWh != 50 {
		t.Errorf("expected_wh should overwrite, got %.0f", got.ExpectedWh)
	}
}

func TestLoadPVPerformanceMissAndRange(t *testing.T) {
	s := freshStore(t)
	if _, ok, err := s.LoadPVPerformanceDay("2000-01-01"); ok || err != nil {
		t.Fatalf("miss should be ok=false err=nil, got ok=%v err=%v", ok, err)
	}
	for _, d := range []string{"2026-06-19", "2026-06-20", "2026-06-21"} {
		_ = s.SavePVPerformance(PVPerformanceDay{Day: d, ExpectedWh: 1000, ActualWh: 900, PR: f64(0.9)})
	}
	got, err := s.LoadPVPerformance("2026-06-20", "2026-06-21")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Day != "2026-06-20" || got[1].Day != "2026-06-21" {
		t.Fatalf("range/order wrong: %+v", got)
	}
}
