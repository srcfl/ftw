package state

import (
	"math"
	"testing"
)

// approxEq compares two float64 values within tol absolute error.
func approxEq(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestDailyCostBreakdown_LoadBaselineCountsAvoidedImportAndExport(t *testing.T) {
	s := freshStore(t)

	// Two 1-hour price slots covering [0, 7200000) ms.
	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 80, TotalOreKwh: 100, Source: "test", FetchedAtMs: 0},
		{Zone: "SE3", SlotTsMs: 3600000, SlotLenMin: 60, SpotOreKwh: 150, TotalOreKwh: 200, Source: "test", FetchedAtMs: 0},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}

	// The house uses 1 kW for both hours. In hour 1 PV covers the load
	// exactly, so actual grid is zero. In hour 2 PV exceeds load and exports
	// 0.5 kW. The no-PV/no-battery baseline is still the full 2 kWh house
	// load bought from the grid.
	if err := s.BulkRecordHistory([]HistoryPoint{
		{TsMs: 0, GridW: 0, LoadW: 1000},
		{TsMs: 3_600_000, GridW: 0, LoadW: 1000},
		{TsMs: 7_200_000, GridW: -500, LoadW: 1000},
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	b, err := s.DailyCostBreakdown(0, 7_200_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}

	if !approxEq(b.LoadWh, 2000, 0.01) {
		t.Errorf("LoadWh = %.4f, want 2000", b.LoadWh)
	}
	if !approxEq(b.ImportWh, 0, 0.01) {
		t.Errorf("ImportWh = %.4f, want 0", b.ImportWh)
	}
	if !approxEq(b.ExportWh, 500, 0.01) {
		t.Errorf("ExportWh = %.4f, want 500", b.ExportWh)
	}

	if !approxEq(b.ImportCostOre, 0, 0.01) {
		t.Errorf("ImportCostOre = %.4f, want 0", b.ImportCostOre)
	}
	if !approxEq(b.ExportRevenueOre, 75, 0.01) {
		t.Errorf("ExportRevenueOre = %.4f, want 75", b.ExportRevenueOre)
	}
	if !approxEq(b.BaselineCostOre, 300, 0.01) {
		t.Errorf("BaselineCostOre = %.4f, want 300", b.BaselineCostOre)
	}
	if !approxEq(b.ActualCostOre(), -75, 0.01) {
		t.Errorf("ActualCostOre = %.4f, want -75", b.ActualCostOre())
	}
	if !approxEq(b.SavedOre(), 375, 0.01) {
		t.Errorf("SavedOre = %.4f, want 375", b.SavedOre())
	}
	if !approxEq(b.FlatCostOre(), b.BaselineCostOre, 0.01) {
		t.Errorf("FlatCostOre compatibility = %.4f, want baseline %.4f", b.FlatCostOre(), b.BaselineCostOre)
	}
	if b.PriceSlotCount != 2 {
		t.Errorf("PriceSlotCount = %d, want 2", b.PriceSlotCount)
	}
}

func TestDailyCostBreakdown_NoPVBatteryBaselineMatchesActualImport(t *testing.T) {
	s := freshStore(t)

	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 80, TotalOreKwh: 100, Source: "test", FetchedAtMs: 0},
		{Zone: "SE3", SlotTsMs: 3600000, SlotLenMin: 60, SpotOreKwh: 150, TotalOreKwh: 200, Source: "test", FetchedAtMs: 0},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}
	if err := s.BulkRecordHistory([]HistoryPoint{
		{TsMs: 0, GridW: 1000, LoadW: 1000},
		{TsMs: 3_600_000, GridW: 1000, LoadW: 1000},
		{TsMs: 7_200_000, GridW: 1000, LoadW: 1000},
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	b, err := s.DailyCostBreakdown(0, 7_200_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if !approxEq(b.BaselineCostOre, 300, 0.01) {
		t.Errorf("BaselineCostOre = %.4f, want 300", b.BaselineCostOre)
	}
	if !approxEq(b.ActualCostOre(), 300, 0.01) {
		t.Errorf("ActualCostOre = %.4f, want 300", b.ActualCostOre())
	}
	if !approxEq(b.SavedOre(), 0, 0.01) {
		t.Errorf("SavedOre = %.4f, want 0", b.SavedOre())
	}
}

func TestDailyCostBreakdown_NegativeExportCostsByDefault(t *testing.T) {
	s := freshStore(t)

	// Single slot with NEGATIVE spot price. Without an explicit floor,
	// export is negative revenue: pay-to-export.
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
	wantWh := 5.0 * 1000.0 / 6.0 // first sample seeds LAG and is dropped
	wantRev := wantWh * -55.0 / 1000.0
	if !approxEq(b.ExportRevenueOre, wantRev, 0.01) {
		t.Errorf("ExportRevenueOre = %.4f, want %.4f (negative export revenue)", b.ExportRevenueOre, wantRev)
	}
	if !approxEq(b.ActualCostOre(), -wantRev, 0.01) {
		t.Errorf("ActualCostOre = %.4f, want %.4f", b.ActualCostOre(), -wantRev)
	}
	if !approxEq(b.AvgExportOreKwh, -55, 0.001) {
		t.Errorf("AvgExportOreKwh = %.4f, want -55", b.AvgExportOreKwh)
	}
}

func TestDailyCostBreakdown_ExportFloorClampsAtZero(t *testing.T) {
	s := freshStore(t)

	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: -50, TotalOreKwh: 30, Source: "test", FetchedAtMs: 0},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}
	pts := []HistoryPoint{}
	for i := 1; i <= 6; i++ {
		pts = append(pts, HistoryPoint{TsMs: int64(i) * 600_000, GridW: -1000})
	}
	if err := s.BulkRecordHistory(pts); err != nil {
		t.Fatalf("record history: %v", err)
	}

	zero := 0.0
	ep := ExportPricing{BonusOreKwh: 5, FeeOreKwh: 10, FloorOreKwh: &zero}
	b, err := s.DailyCostBreakdown(0, 3_600_000, "SE3", ep)
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if b.ExportRevenueOre != 0 {
		t.Errorf("ExportRevenueOre = %.4f, want 0 under export floor", b.ExportRevenueOre)
	}
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

func TestDailyCostBreakdown_FlatAverageIsHalfOpenAndTimeWeighted(t *testing.T) {
	s := freshStore(t)

	if err := s.SavePrices([]PricePoint{
		// Overlaps the first 30 min of the query range.
		{Zone: "SE3", SlotTsMs: -1_800_000, SlotLenMin: 60, SpotOreKwh: 80, TotalOreKwh: 100, Source: "test", FetchedAtMs: 0},
		// Overlaps the second 30 min of the query range.
		{Zone: "SE3", SlotTsMs: 1_800_000, SlotLenMin: 60, SpotOreKwh: 160, TotalOreKwh: 300, Source: "test", FetchedAtMs: 0},
		// Starts exactly at untilMs; must not leak into the half-open range.
		{Zone: "SE3", SlotTsMs: 3_600_000, SlotLenMin: 60, SpotOreKwh: 9000, TotalOreKwh: 9000, Source: "test", FetchedAtMs: 0},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}
	if err := s.BulkRecordHistory([]HistoryPoint{
		{TsMs: 0, GridW: 1000},
		{TsMs: 600_000, GridW: 1000},
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	b, err := s.DailyCostBreakdown(0, 3_600_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}

	if !approxEq(b.AvgImportOreKwh, 200, 0.01) {
		t.Errorf("AvgImportOreKwh = %.4f, want 200", b.AvgImportOreKwh)
	}
	if !approxEq(b.AvgExportOreKwh, 120, 0.01) {
		t.Errorf("AvgExportOreKwh = %.4f, want 120", b.AvgExportOreKwh)
	}
	if b.PriceSlotCount != 2 {
		t.Errorf("PriceSlotCount = %d, want 2", b.PriceSlotCount)
	}
}

func TestDailyCostBreakdown_EmptyRange(t *testing.T) {
	s := freshStore(t)
	b, err := s.DailyCostBreakdown(0, 1_000_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown empty: %v", err)
	}
	if b.ImportWh != 0 || b.ExportWh != 0 || b.LoadWh != 0 ||
		b.ImportCostOre != 0 || b.ExportRevenueOre != 0 || b.BaselineCostOre != 0 {
		t.Errorf("empty range returned non-zero breakdown: %+v", b)
	}
	if b.PriceSlotCount != 0 {
		t.Errorf("PriceSlotCount = %d, want 0", b.PriceSlotCount)
	}
}
