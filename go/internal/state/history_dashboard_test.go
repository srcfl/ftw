package state

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func dashboardTick(t *testing.T, s *Store, id string, p *HistoryPoint) {
	t.Helper()
	if _, err := s.recordHistoryBatch(context.Background(), id, id, p, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
}
func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-8 {
		t.Fatalf("%s=%v want %v", name, got, want)
	}
}

func TestDashboardEnergyAndCostUseOriginalIntervals(t *testing.T) {
	s, raw := freshStore(t), freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour)
	ms := base.UnixMilli()
	offsets := []int64{0, 1000, 8000, 10000, 59000, 60000, 61000, 120000}
	for i, off := range offsets {
		p := HistoryPoint{TsMs: ms + off, GridW: 6000, PVW: -1000, BatW: 100, LoadW: 2000, JSON: fmt.Sprintf(`{"index":%d}`, i)}
		if i%2 != 0 {
			p.GridW = -3000
			p.BatW = -100
		}
		dashboardTick(t, s, fmt.Sprint(i), &p)
		if err := raw.RecordHistory(p); err != nil {
			t.Fatal(err)
		}
	}
	want, err := raw.DailyEnergy(ms, ms+120000)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DailyEnergy(ms, ms+120000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "import", got.ImportWh, want.ImportWh)
	near(t, "export", got.ExportWh, want.ExportWh)
	near(t, "PV", got.PVWh, want.PVWh)
	near(t, "battery charge", got.BatChargedWh, want.BatChargedWh)
	near(t, "battery discharge", got.BatDischargedWh, want.BatDischargedWh)
	near(t, "load", got.LoadWh, want.LoadWh)
	slots := []priceSlot{{StartMs: ms, EndMs: ms + 120000, TotalOre: 200, SpotOreKwh: 100}}
	a, err := s.integrateHistoryRange(context.Background(), ms, ms+120000, slots, ExportPricing{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := raw.integrateHistoryRange(context.Background(), ms, ms+120000, slots, ExportPricing{})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "cost", a.ImportCostOre, b.ImportCostOre)
	near(t, "export revenue", a.ExportRevenueOre, b.ExportRevenueOre)
	near(t, "EV", a.EVWh, b.EVWh)
	near(t, "house baseline", a.BaselineHouseOre, b.BaselineHouseOre)
	if a.HistoryCoveredMs != 120000 || a.PricedCoveredMs != 120000 {
		t.Fatal(a)
	}
	var hot int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&hot); err != nil || hot != 0 {
		t.Fatal("retained raw chart ticks", hot, err)
	}
	for _, age := range []time.Duration{48 * time.Hour, 40 * 24 * time.Hour, 732 * 24 * time.Hour} {
		if err := s.maintainDashboard(context.Background(), base.Add(age)); err != nil {
			t.Fatal(err)
		}
		after, err := s.DailyEnergy(ms, ms+120000)
		if err != nil {
			t.Fatal(err)
		}
		near(t, "retained import", after.ImportWh, want.ImportWh)
		near(t, "retained export", after.ExportWh, want.ExportWh)
		cost, err := s.integrateHistoryRange(context.Background(), ms, ms+120000, slots, ExportPricing{})
		if err != nil {
			t.Fatal(err)
		}
		near(t, "retained cost", cost.ImportCostOre, a.ImportCostOre)
	}
}

func TestDashboardWeightedRollupAndNewestJSON(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(24 * time.Hour)
	ms := base.UnixMilli()
	for i, off := range []int64{1000, 2000, 3000, 59000, 61000} {
		p := HistoryPoint{TsMs: ms + off, GridW: float64(i * 100), JSON: fmt.Sprintf(`{"index":%d}`, i)}
		dashboardTick(t, s, fmt.Sprint(i), &p)
	}
	check := func(wantWidth int64) {
		t.Helper()
		points, err := s.LoadHistory(ms, ms+120000, 1)
		if err != nil || len(points) != 1 {
			t.Fatal(points, err)
		}
		p := points[0]
		var detail map[string]any
		if err := json.Unmarshal([]byte(p.JSON), &detail); err != nil || detail["index"] != float64(4) || detail["forecast_measurement_quality"] != "aggregate_not_for_training" {
			t.Fatal(p.JSON, err)
		}
		if p.N != 5 || p.GridW != 200 || p.ResolutionMS != wantWidth {
			t.Fatal(p)
		}
	}
	check(HistoryResolutionMS)
	if err := s.maintainDashboard(context.Background(), base.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(ArchiveResolutionMS)
	if err := s.maintainDashboard(context.Background(), base.Add(40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(OldArchiveResolutionMS)
	if err := s.maintainDashboard(context.Background(), base.Add(40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(OldArchiveResolutionMS)
}

func TestDashboardRestartReceiptAndOutage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	a := HistoryPoint{TsMs: base, GridW: 3600, JSON: "{}"}
	b := a
	b.TsMs += 1000
	dashboardTick(t, s, "a", &a)
	dashboardTick(t, s, "b", &b)
	before, err := s.DailyEnergy(base, base+1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	dashboardTick(t, s, "b", &b)
	dashboardTick(t, s, "duplicate-timestamp", &b)
	after, err := s.DailyEnergy(base, base+1000)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatal(before, after, err)
	}
	dashboardTick(t, s, "missing", nil)
	c := a
	c.TsMs = base + 60000
	dashboardTick(t, s, "recovered", &c)
	d := c
	d.TsMs += 1000
	dashboardTick(t, s, "after", &d)
	got, err := s.DailyEnergy(base, base+61000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "outage energy", got.ImportWh, 2)
	cost, err := s.integrateHistoryRange(context.Background(), base, base+61000, nil, ExportPricing{})
	if err != nil {
		t.Fatal(err)
	}
	if cost.HistoryCoveredMs != 2000 || cost.PricedCoveredMs != 0 || cost.EVWh != 0 {
		t.Fatal("filled outage or credited unpriced EV", cost)
	}
	points, err := s.LoadHistory(base, base+61000, 0)
	if err != nil || len(points) != 2 || points[0].N != 2 || points[1].N != 2 {
		t.Fatal(points, err)
	}
}

func TestDashboardTariffBoundaryAndPartialEvidence(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	a := HistoryPoint{TsMs: base + 59000, GridW: 3600, JSON: "{}"}
	b := a
	b.TsMs = base + 61000
	dashboardTick(t, s, "a", &a)
	dashboardTick(t, s, "b", &b)
	slots := []priceSlot{{StartMs: base, EndMs: base + 60000, TotalOre: 100}, {StartMs: base + 60000, EndMs: base + 120000, TotalOre: 300}}
	got, err := s.integrateHistoryRange(context.Background(), base, base+120000, slots, ExportPricing{})
	if err != nil {
		t.Fatal(err)
	}
	near(t, "split tariff energy", got.ImportWh, 2)
	near(t, "split tariff cost", got.ImportCostOre, .4)
	wh, cov, err := s.ImportWhIntervals(context.Background(), [][2]int64{{base, base + 60000}, {base + 60000, base + 120000}})
	if err != nil || len(wh) != 2 || wh[0] != 1 || wh[1] != 1 || cov[0] != 1000 || cov[1] != 1000 {
		t.Fatal(wh, cov, err)
	}
	partial, err := s.integrateHistoryRange(context.Background(), base+59500, base+60500, slots, ExportPricing{})
	if err != nil {
		t.Fatal(err)
	}
	if partial.HistoryCoveredMs != 0 || partial.ImportWh != 0 {
		t.Fatal("invented evidence inside a compacted interval", partial)
	}
}

func TestDashboardTransitionKeepsLegacyEnergy(t *testing.T) {
	s := freshStore(t)
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	for _, off := range []int64{0, 1000} {
		if err := s.RecordHistory(HistoryPoint{TsMs: base + off, GridW: 3600, JSON: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	p := HistoryPoint{TsMs: base + 2000, GridW: -3600, JSON: "{}"}
	dashboardTick(t, s, "new", &p)
	got, err := s.DailyEnergy(base, base+2000)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "legacy import", got.ImportWh, 1)
	near(t, "new export", got.ExportWh, 1)
}

func TestDashboardOldEnergyRetainsLocalDayBoundary(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	loc := time.FixedZone("quarter hour offset", 5*3600+45*60)
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, loc)
	for i, dt := range []time.Duration{-time.Minute, 0, time.Minute} {
		dashboardTick(t, s, fmt.Sprint(i), &HistoryPoint{TsMs: day.Add(dt).UnixMilli(), GridW: 3600, JSON: "{}"})
	}
	if err := s.maintainDashboard(context.Background(), day.Add(800*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, err := s.DailyEnergy(day.Add(-24*time.Hour).UnixMilli(), day.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.DailyEnergy(day.UnixMilli(), day.Add(24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	near(t, "previous local day", before.ImportWh, 60)
	near(t, "next local day", after.ImportWh, 60)
}
