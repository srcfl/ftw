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

// EV charging counter-factual: the EMS schedules the car onto the cheap
// hour, and the baseline must price those kWh at the day's average so the
// scheduling shows up as savings instead of zeroing out.
func TestDailyCostBreakdown_EVChargingPricedAtDailyAverage(t *testing.T) {
	s := freshStore(t)

	// Two 1-h slots, big spread: 0 öre vs 400 öre. Time-weighted avg =
	// 200 öre/kWh.
	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 0, TotalOreKwh: 0, Source: "test"},
		{Zone: "SE3", SlotTsMs: 3600000, SlotLenMin: 60, SpotOreKwh: 400, TotalOreKwh: 400, Source: "test"},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}

	// Hour 1 (0 öre): house = 0, EV charges at 11 kW → grid imports 11 kW.
	// Hour 2 (400 öre): house = 0, EV off → grid 0.
	// House load_w is the "house minus EV" component; EV is derived as
	// grid_w − bat_w − pv_w − load_w = 11000 in hour 1, 0 in hour 2.
	if err := s.BulkRecordHistory([]HistoryPoint{
		{TsMs: 0, GridW: 11000, LoadW: 0},
		{TsMs: 3_600_000, GridW: 11000, LoadW: 0},
		{TsMs: 7_200_000, GridW: 0, LoadW: 0},
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	b, err := s.DailyCostBreakdown(0, 7_200_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}

	// 11 kW × 1 h = 11 kWh of EV, no house load.
	if !approxEq(b.EVWh, 11_000, 0.01) {
		t.Errorf("EVWh = %.4f, want 11000", b.EVWh)
	}
	if !approxEq(b.LoadWh, 0, 0.01) {
		t.Errorf("LoadWh = %.4f, want 0 (no house load)", b.LoadWh)
	}
	// Daily avg = (0 × 1h + 400 × 1h) / 2h = 200 öre/kWh.
	if !approxEq(b.AvgImportOreKwh, 200, 0.01) {
		t.Errorf("AvgImportOreKwh = %.4f, want 200", b.AvgImportOreKwh)
	}
	// House baseline: 0. EV baseline: 11 kWh × 200 öre = 2200 öre.
	if !approxEq(b.BaselineHouseOre, 0, 0.01) {
		t.Errorf("BaselineHouseOre = %.4f, want 0", b.BaselineHouseOre)
	}
	if !approxEq(b.BaselineEvOre, 2200, 0.01) {
		t.Errorf("BaselineEvOre = %.4f, want 2200", b.BaselineEvOre)
	}
	if !approxEq(b.BaselineCostOre, 2200, 0.01) {
		t.Errorf("BaselineCostOre = %.4f, want 2200", b.BaselineCostOre)
	}
	// Actual: 11 kWh at 0 öre = 0.
	if !approxEq(b.ActualCostOre(), 0, 0.01) {
		t.Errorf("ActualCostOre = %.4f, want 0", b.ActualCostOre())
	}
	// Saved: full 2200 öre — the EMS picked the 0-öre hour.
	if !approxEq(b.SavedOre(), 2200, 0.01) {
		t.Errorf("SavedOre = %.4f, want 2200 (full daily-avg credit)", b.SavedOre())
	}
}

// Regression: EV energy charged during a price GAP (no covering slot) must
// NOT inflate the EV baseline. Before the fix, EVWh counted uncovered
// samples and BaselineEvOre priced them at the daily average — manufacturing
// savings with no matching actual import cost. The house baseline already
// gates on coverage (BaselineHouseOre only accrues when covering != nil); EV
// must too. Mirrors the rationale in cost.go.
func TestDailyCostBreakdown_EVInPriceGapExcludedFromBaseline(t *testing.T) {
	s := freshStore(t)

	// Only ONE slot: hour 1 [0, 3.6M) at 100 öre. Hour 2 [3.6M, 7.2M) has
	// no price slot — a gap.
	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 100, TotalOreKwh: 100, Source: "test"},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}

	// EV charges 11 kW across BOTH hours (grid imports 11 kW, no house load):
	//   row @3.6M → EV over (0, 3.6M]   midpoint 1.8M  → COVERED
	//   row @7.2M → EV over (3.6M, 7.2M] midpoint 5.4M → GAP (no slot)
	if err := s.BulkRecordHistory([]HistoryPoint{
		{TsMs: 0, GridW: 11000, LoadW: 0},
		{TsMs: 3_600_000, GridW: 11000, LoadW: 0},
		{TsMs: 7_200_000, GridW: 11000, LoadW: 0},
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	b, err := s.DailyCostBreakdown(0, 7_200_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}

	// Only the covered hour's 11 kWh of EV counts toward the priced baseline;
	// the gap hour's 11 kWh must be excluded (no phantom savings).
	if !approxEq(b.EVWh, 11_000, 0.01) {
		t.Errorf("EVWh = %.4f, want 11000 (gap-hour EV excluded)", b.EVWh)
	}
	// avg import = 100 öre (single slot). Baseline EV = 11 kWh × 100 = 1100 öre.
	if !approxEq(b.BaselineEvOre, 1100, 0.01) {
		t.Errorf("BaselineEvOre = %.4f, want 1100 (gap EV must not inflate baseline)", b.BaselineEvOre)
	}
}

// Dumb EV charging on the expensive hour must show up as a loss vs the
// daily-average baseline — the metric is symmetric and doesn't only credit
// good decisions.
func TestDailyCostBreakdown_EVOnExpensiveHourPaysVsDailyAverage(t *testing.T) {
	s := freshStore(t)

	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 0, TotalOreKwh: 0, Source: "test"},
		{Zone: "SE3", SlotTsMs: 3600000, SlotLenMin: 60, SpotOreKwh: 400, TotalOreKwh: 400, Source: "test"},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}
	// EV charges only on the 400-öre hour.
	if err := s.BulkRecordHistory([]HistoryPoint{
		{TsMs: 0, GridW: 0, LoadW: 0},
		{TsMs: 3_600_000, GridW: 0, LoadW: 0},
		{TsMs: 7_200_000, GridW: 11000, LoadW: 0},
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	b, err := s.DailyCostBreakdown(0, 7_200_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}

	// 11 kW for 1 hour in slot 2 → 11 kWh.
	if !approxEq(b.EVWh, 11_000, 0.01) {
		t.Errorf("EVWh = %.4f, want 11000", b.EVWh)
	}
	// Baseline 11 × 200 = 2200, Actual 11 × 400 = 4400, Saved = −2200.
	if !approxEq(b.BaselineEvOre, 2200, 0.01) {
		t.Errorf("BaselineEvOre = %.4f, want 2200", b.BaselineEvOre)
	}
	if !approxEq(b.ActualCostOre(), 4400, 0.01) {
		t.Errorf("ActualCostOre = %.4f, want 4400", b.ActualCostOre())
	}
	if !approxEq(b.SavedOre(), -2200, 0.01) {
		t.Errorf("SavedOre = %.4f, want -2200 (penalty for charging on peak)", b.SavedOre())
	}
}

// Battery charging from grid is not EV — verify we don't double-count it
// as EV in the derivation `grid − bat − pv − load`.
func TestDailyCostBreakdown_BatteryChargeNotMistakenForEV(t *testing.T) {
	s := freshStore(t)

	if err := s.SavePrices([]PricePoint{
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 100, TotalOreKwh: 100, Source: "test"},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}
	// House 1 kW, battery charging 5 kW from grid → grid 6 kW import.
	// load_w (house only) = 1000, bat_w = 5000, ev_w derived = 0.
	if err := s.BulkRecordHistory([]HistoryPoint{
		{TsMs: 0, GridW: 6000, LoadW: 1000, BatW: 5000},
		{TsMs: 3_600_000, GridW: 6000, LoadW: 1000, BatW: 5000},
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	b, err := s.DailyCostBreakdown(0, 3_600_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if !approxEq(b.EVWh, 0, 0.01) {
		t.Errorf("EVWh = %.4f, want 0 (battery charge isn't EV)", b.EVWh)
	}
	if !approxEq(b.BaselineEvOre, 0, 0.01) {
		t.Errorf("BaselineEvOre = %.4f, want 0", b.BaselineEvOre)
	}
}

// PV self-consumption + battery dispatch + EV charging in one fixture, to
// make sure the three credits stack the way the comment in DayCostBreakdown
// claims.
func TestDailyCostBreakdown_PVBatteryAndEVStack(t *testing.T) {
	s := freshStore(t)

	if err := s.SavePrices([]PricePoint{
		// Hour 1: cheap, EMS charges battery + EV here.
		{Zone: "SE3", SlotTsMs: 0, SlotLenMin: 60, SpotOreKwh: 50, TotalOreKwh: 50, Source: "test"},
		// Hour 2: expensive, battery discharges to cover house load.
		{Zone: "SE3", SlotTsMs: 3600000, SlotLenMin: 60, SpotOreKwh: 250, TotalOreKwh: 250, Source: "test"},
	}); err != nil {
		t.Fatalf("save prices: %v", err)
	}
	// Hour 1 (cheap):
	//   house  = 1 kW,  EV    = 11 kW,  bat = +5 kW (charging), pv = 0
	//   grid_w = 1 + 11 + 5 = 17 kW import.
	// Hour 2 (expensive):
	//   house  = 1 kW,  EV    =  0,     bat = −1 kW (discharge), pv = 0
	//   grid_w = 1 + 0 + (−1) = 0.
	if err := s.BulkRecordHistory([]HistoryPoint{
		{TsMs: 0, GridW: 17000, LoadW: 1000, BatW: 5000},
		{TsMs: 3_600_000, GridW: 17000, LoadW: 1000, BatW: 5000},
		{TsMs: 7_200_000, GridW: 0, LoadW: 1000, BatW: -1000},
	}); err != nil {
		t.Fatalf("record history: %v", err)
	}

	b, err := s.DailyCostBreakdown(0, 7_200_000, "SE3", ExportPricing{})
	if err != nil {
		t.Fatalf("breakdown: %v", err)
	}
	if !approxEq(b.EVWh, 11_000, 0.01) {
		t.Errorf("EVWh = %.4f, want 11000", b.EVWh)
	}
	if !approxEq(b.LoadWh, 2000, 0.01) {
		t.Errorf("LoadWh = %.4f, want 2000", b.LoadWh)
	}
	// Daily avg = (50+250)/2 = 150 öre/kWh.
	if !approxEq(b.AvgImportOreKwh, 150, 0.01) {
		t.Errorf("AvgImportOreKwh = %.4f, want 150", b.AvgImportOreKwh)
	}
	// Baselines:
	//   house: 1 kWh × 50 + 1 kWh × 250 = 300 öre.
	//   ev   : 11 kWh × 150 = 1650 öre.
	//   total = 1950 öre.
	if !approxEq(b.BaselineHouseOre, 300, 0.01) {
		t.Errorf("BaselineHouseOre = %.4f, want 300", b.BaselineHouseOre)
	}
	if !approxEq(b.BaselineEvOre, 1650, 0.01) {
		t.Errorf("BaselineEvOre = %.4f, want 1650", b.BaselineEvOre)
	}
	if !approxEq(b.BaselineCostOre, 1950, 0.01) {
		t.Errorf("BaselineCostOre = %.4f, want 1950", b.BaselineCostOre)
	}
	// Actual: 17 kWh × 50 = 850 öre on hour 1, 0 on hour 2.
	if !approxEq(b.ActualCostOre(), 850, 0.01) {
		t.Errorf("ActualCostOre = %.4f, want 850", b.ActualCostOre())
	}
	// Saved = 1950 − 850 = 1100 öre — EV scheduling + battery shifting
	// + cheap-hour charging all contribute.
	if !approxEq(b.SavedOre(), 1100, 0.01) {
		t.Errorf("SavedOre = %.4f, want 1100", b.SavedOre())
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
