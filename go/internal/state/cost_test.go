package state

import (
	"math"
	"testing"
)

// approxEq compares two float64 values within tol absolute error.
func approxEq(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestDailyCostBreakdown_SlotWeightedAgainstFlat(t *testing.T) {
	s := freshStore(t)

	// Two 1-hour price slots covering [0, 7200000) ms.
	// Slot 0 is cheap, slot 1 is expensive — so importing in slot 0 and
	// exporting in slot 1 is the *timing-winning* scenario; the planner
	// should look better than flat-rate.
	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 80, TotalOreKwh: 100, Source: "test", FetchedAtMs: 0},
		{Zone: "SE3", SlotTsMs: 3600000, SlotLenMin: 60, SpotOreKwh: 150, TotalOreKwh: 200, Source: "test", FetchedAtMs: 0},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}

	// History samples every 10 min = 600_000 ms.
	// Slot 0 (mid-times 300k..3300k all inside [0, 3600000)):
	//   ts 600k, 1200k, 1800k, 2400k, 3000k, 3600k — grid_w = +1000 W (import 1 kW)
	// Slot 1 (mid-times 3900k..6900k all inside [3600000, 7200000)):
	//   ts 4200k, 4800k, 5400k, 6000k, 6600k, 7200k — grid_w = -2000 W (export 2 kW)
	//
	// Each integration step covers dt = 600_000 ms = 1/6 h.
	// Slot 0: 6 steps × 1000 W × 1/6 h = 1000 Wh import.
	// Slot 1: 6 steps × 2000 W × 1/6 h = 2000 Wh export.
	pts := []HistoryPoint{}
	for i := 1; i <= 6; i++ {
		pts = append(pts, HistoryPoint{TsMs: int64(i) * 600_000, GridW: 1000})
	}
	for i := 7; i <= 12; i++ {
		pts = append(pts, HistoryPoint{TsMs: int64(i) * 600_000, GridW: -2000})
	}
	if err := s.BulkRecordHistory(pts); err != nil {
		t.Fatalf("record history: %v", err)
	}

	ep := ExportPricing{} // no bonus/fee/flat → export = spot, clamped at 0

	b, err := s.DailyCostBreakdown(0, 7_200_000, "SE3", ep)
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}

	// Volumes: the first sample (ts=600k) has prev_ts=NULL after LAG and
	// is dropped; integration starts at the second sample. So we lose one
	// 10-min step of slot-0 import → 5 steps × 1 kW × 1/6 h = 833.33 Wh.
	// All 6 export samples have a previous sample (the last import sample
	// at ts=3600k is their predecessor) → 6 steps × 2 kW × 1/6 h = 2000 Wh.
	wantImpWh := 5.0 * 1000.0 / 6.0
	wantExpWh := 6.0 * 2000.0 / 6.0
	if !approxEq(b.ImportWh, wantImpWh, 0.01) {
		t.Errorf("ImportWh = %.4f, want %.4f", b.ImportWh, wantImpWh)
	}
	if !approxEq(b.ExportWh, wantExpWh, 0.01) {
		t.Errorf("ExportWh = %.4f, want %.4f", b.ExportWh, wantExpWh)
	}

	// First export sample (ts=4200k, prev=3600k) midpoint = 3900k → slot 1.
	// All export samples are priced at slot 1 (spot=150, no bonus/fee).
	// First import-after-NULL sample drops out; remaining import samples are
	// all priced at slot 0 (total=100). One step crosses the slot boundary:
	// sample at ts=3600k, prev=3000k, mid=3300k → slot 0. Good.
	//
	// import_cost = 833.33 Wh × 100 öre/kWh / 1000 = 83.333 öre
	// export_rev  = 2000 Wh × 150 öre/kWh / 1000   = 300 öre
	wantImpCost := wantImpWh * 100.0 / 1000.0
	wantExpRev := wantExpWh * 150.0 / 1000.0
	if !approxEq(b.ImportCostOre, wantImpCost, 0.01) {
		t.Errorf("ImportCostOre = %.4f, want %.4f", b.ImportCostOre, wantImpCost)
	}
	if !approxEq(b.ExportRevenueOre, wantExpRev, 0.01) {
		t.Errorf("ExportRevenueOre = %.4f, want %.4f", b.ExportRevenueOre, wantExpRev)
	}

	// Avg prices are unweighted means over slots in range — mirrors
	// mpc.ComputeBaselines' flat-avg pricing.
	// avg_import = (100 + 200) / 2 = 150
	// avg_export = (80 + 150) / 2 = 115
	if !approxEq(b.AvgImportOreKwh, 150, 0.01) {
		t.Errorf("AvgImportOreKwh = %.4f, want 150", b.AvgImportOreKwh)
	}
	if !approxEq(b.AvgExportOreKwh, 115, 0.01) {
		t.Errorf("AvgExportOreKwh = %.4f, want 115", b.AvgExportOreKwh)
	}

	// Derived ergonomics:
	//   actual = 83.333 - 300 = -216.667 (net earned 216.667 öre)
	//   flat   = (833.33 * 150 - 2000 * 115) / 1000 = (125000 - 230000) / 1000 = -105 öre
	//   saved  = flat - actual = -105 - (-216.667) = 111.667 öre  (timing won)
	wantActual := wantImpCost - wantExpRev
	wantFlat := (wantImpWh*150.0 - wantExpWh*115.0) / 1000.0
	wantSaved := wantFlat - wantActual
	if !approxEq(b.ActualCostOre(), wantActual, 0.01) {
		t.Errorf("ActualCostOre = %.4f, want %.4f", b.ActualCostOre(), wantActual)
	}
	if !approxEq(b.FlatCostOre(), wantFlat, 0.01) {
		t.Errorf("FlatCostOre = %.4f, want %.4f", b.FlatCostOre(), wantFlat)
	}
	if !approxEq(b.SavedOre(), wantSaved, 0.01) {
		t.Errorf("SavedOre = %.4f, want %.4f", b.SavedOre(), wantSaved)
	}
	if wantSaved <= 0 {
		t.Fatalf("test scenario malformed — flat should beat actual by a positive margin, got %.2f", wantSaved)
	}
}

func TestDailyCostBreakdown_ExportClampedAtZero(t *testing.T) {
	s := freshStore(t)

	// Single slot with NEGATIVE spot price — export should earn 0 (clamped),
	// not pay-to-export.
	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: -50, TotalOreKwh: 30, Source: "test", FetchedAtMs: 0},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}

	// Just export, no import.
	pts := []HistoryPoint{}
	for i := 1; i <= 6; i++ {
		pts = append(pts, HistoryPoint{TsMs: int64(i) * 600_000, GridW: -1000})
	}
	if err := s.BulkRecordHistory(pts); err != nil {
		t.Fatalf("record history: %v", err)
	}

	ep := ExportPricing{BonusOreKwh: 5, FeeOreKwh: 10} // bonus 5 - fee 10 on top of -50 → still ≤ 0

	b, err := s.DailyCostBreakdown(0, 3_600_000, "SE3", ep)
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if b.ExportRevenueOre != 0 {
		t.Errorf("ExportRevenueOre = %.4f, want 0 (clamped on negative net export price)", b.ExportRevenueOre)
	}
	// AvgExportOreKwh should also be the clamped value (0) for the one slot.
	if !approxEq(b.AvgExportOreKwh, 0, 0.001) {
		t.Errorf("AvgExportOreKwh = %.4f, want 0", b.AvgExportOreKwh)
	}
}

func TestDailyCostBreakdown_FlatExportOverride(t *testing.T) {
	s := freshStore(t)

	// When ExportPricing.FlatOreKwh > 0, it must override the spot model
	// entirely (mirrors mpc.SlotExportPriceOre's ExportOrePerKWh branch).
	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 200, TotalOreKwh: 250, Source: "test", FetchedAtMs: 0},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}
	pts := []HistoryPoint{
		{TsMs: 600_000, GridW: -1000},
		{TsMs: 1_200_000, GridW: -1000},
	}
	if err := s.BulkRecordHistory(pts); err != nil {
		t.Fatalf("record history: %v", err)
	}

	ep := ExportPricing{FlatOreKwh: 42}
	b, err := s.DailyCostBreakdown(0, 3_600_000, "SE3", ep)
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	// One step survives LAG (ts=1200k, prev=600k), 1 kW × 1/6 h = 166.67 Wh
	// × 42 öre/kWh / 1000 = 7.0 öre.
	wantRev := (1000.0 / 6.0) * 42.0 / 1000.0
	if !approxEq(b.ExportRevenueOre, wantRev, 0.01) {
		t.Errorf("ExportRevenueOre = %.4f, want %.4f", b.ExportRevenueOre, wantRev)
	}
	if !approxEq(b.AvgExportOreKwh, 42, 0.01) {
		t.Errorf("AvgExportOreKwh = %.4f, want 42 (flat override)", b.AvgExportOreKwh)
	}
}

func TestDailyCostBreakdown_EmptyRange(t *testing.T) {
	s := freshStore(t)
	b, err := s.DailyCostBreakdown(0, 1_000_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown empty: %v", err)
	}
	if b.ImportWh != 0 || b.ExportWh != 0 || b.ImportCostOre != 0 || b.ExportRevenueOre != 0 {
		t.Errorf("empty range returned non-zero breakdown: %+v", b)
	}
}
