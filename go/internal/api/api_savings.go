package api

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/savings"
	"github.com/srcfl/ftw/go/internal/state"
)

const (
	savingsValueScope = "site_total"
	savingsBaseline   = "no_pv_no_battery_vehicle_energy_at_daily_average"
	// A built-in self-use mode on the same cells runs a bit below the
	// planner's nameplate efficiency and keeps a backup it will not spend.
	dumbEffHaircut   = 0.05
	dumbEffFloor     = 0.80
	dumbReserveFrac  = 0.10
	batterySOCMetric = "battery_soc"
)

// daySavings is the cached per-local-day cost breakdown that powers
// /api/savings/daily. Mirrors the immutable-day pattern dailyCache uses.
// Past days never re-render; only today is recomputed each request.
type daySavings struct {
	ImportWh         float64
	ExportWh         float64
	LoadWh           float64
	EVWh             float64
	ImportCostOre    float64
	ExportRevenueOre float64
	BaselineHouseOre float64
	BaselineEvOre    float64
	BaselineCostOre  float64
	AvgImportOreKwh  float64
	AvgExportOreKwh  float64
	PriceSlotCount   int
	ActualCostOre    float64
	FlatCostOre      float64
	SavedOre         float64
	ExpectedMs       int64
	HistoryCoveredMs int64
	PricedCoveredMs  int64
	HistoryCoverage  float64
	PricedCoverage   float64
	Resolution       string // "slot" or "no_prices"
}

func fromBreakdown(b state.DayCostBreakdown, resolution string) daySavings {
	return daySavings{
		ImportWh:         b.ImportWh,
		ExportWh:         b.ExportWh,
		LoadWh:           b.LoadWh,
		EVWh:             b.EVWh,
		ImportCostOre:    b.ImportCostOre,
		ExportRevenueOre: b.ExportRevenueOre,
		BaselineHouseOre: b.BaselineHouseOre,
		BaselineEvOre:    b.BaselineEvOre,
		BaselineCostOre:  b.BaselineCostOre,
		AvgImportOreKwh:  b.AvgImportOreKwh,
		AvgExportOreKwh:  b.AvgExportOreKwh,
		PriceSlotCount:   b.PriceSlotCount,
		ActualCostOre:    b.ActualCostOre(),
		FlatCostOre:      b.FlatCostOre(),
		SavedOre:         b.SavedOre(),
		ExpectedMs:       b.ExpectedMs,
		HistoryCoveredMs: b.HistoryCoveredMs,
		PricedCoveredMs:  b.PricedCoveredMs,
		HistoryCoverage:  b.HistoryCoveragePct(),
		PricedCoverage:   b.PricedCoveragePct(),
		Resolution:       resolution,
	}
}

// savingsCache is created lazily on first request. Process-lifetime.
// Keyed on YYYY-MM-DD; immutable days are cached forever. Cleared on
// process restart, which is the only practical way config-driven
// export-pricing changes invalidate it — operators changing
// cfg.Price.ExportBonusOreKwh mid-run will see stale historical answers
// until a restart. Acceptable for an MVP — those fields rarely change.
type savingsCacheT struct {
	mu sync.Mutex
	m  map[string]daySavings
}

// handleSavingsDaily returns per-local-day actual net cost vs the load-only
// no-PV/no-battery baseline, with vehicle energy priced at the day's average.
// This is combined site value, not incremental optimizer value. The endpoint
// name and existing fields are kept for compatibility.
//
// GET /api/savings/daily?days=N
//
// Response:
//
//	{
//	  "days": [
//	    {
//	      "day": "YYYY-MM-DD",
//	      "import_wh": ..., "export_wh": ..., "load_wh": ...,
//	      "import_cost_ore": ..., "export_revenue_ore": ...,
//	      "actual_cost_ore": ..., "baseline_cost_ore": ..., "saved_ore": ...,
//	      "avg_import_ore_kwh": ..., "avg_export_ore_kwh": ...,
//	      "expected_ms": ..., "history_covered_ms": ..., "priced_covered_ms": ...,
//	      "history_coverage_pct": ..., "priced_coverage_pct": ...,
//	      "resolution": "slot" | "no_prices"
//	    },
//	    ...
//	  ],
//	  "totals": { "import_wh": ..., "export_wh": ..., "load_wh": ...,
//	              "actual_cost_ore": ..., "baseline_cost_ore": ..., "saved_ore": ...,
//	              "expected_ms": ..., "history_covered_ms": ..., "priced_covered_ms": ...,
//	              "history_coverage_pct": ..., "priced_coverage_pct": ... },
//	  "tz": "Local", "value_scope": "site_total",
//	  "baseline": "no_pv_no_battery_vehicle_energy_at_daily_average"
//	}
//
// Days where the prices table has no slot for the zone come back with
// resolution="no_prices" and zeroed costs. Volume columns are still
// populated for those days so the UI can distinguish "no data" from
// "data but no prices yet".
func (s *Server) handleSavingsDaily(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, map[string]any{
			"days": []any{}, "value_scope": savingsValueScope, "baseline": savingsBaseline,
		})
		return
	}

	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 90 {
		days = 90
	}

	// Pull export pricing + zone from current config. Take the config
	// mutex briefly to copy the small set of scalars we need so handler
	// work doesn't block hot-path readers.
	zone := ""
	ep := state.ExportPricing{}
	if s.deps.CfgMu != nil && s.deps.Cfg != nil {
		s.deps.CfgMu.RLock()
		if s.deps.Cfg.Price != nil {
			zone = s.deps.Cfg.Price.Zone
			ep.BonusOreKwh = s.deps.Cfg.Price.ExportBonusOreKwh
			ep.FeeOreKwh = s.deps.Cfg.Price.ExportFeeOreKwh
			ep.FloorOreKwh = s.deps.Cfg.Price.ExportFloorOreKwh
		}
		if s.deps.Cfg.Planner != nil {
			ep.FlatOreKwh = s.deps.Cfg.Planner.ExportOrePerKWh
		}
		s.deps.CfgMu.RUnlock()
	}
	if zone == "" {
		// No price provider configured → nothing to compare against.
		writeJSON(w, 200, map[string]any{
			"days": []any{}, "tz": time.Now().Location().String(),
			"value_scope": savingsValueScope, "baseline": savingsBaseline,
		})
		return
	}

	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	if wrote, err := s.writeLedgerSavings(w, r, days, now, loc, todayMidnight, zone, ep); wrote || err != nil {
		if err != nil {
			slog.Error("handleSavingsDaily: ledger savings failed", "err", err)
			http.Error(w, "savings load failed", http.StatusInternalServerError)
		}
		return
	}

	s.ensureSavingsCache()

	out := make([]map[string]any, 0, days)
	var tImpWh, tExpWh, tLoadWh, tActual, tBaseline, tSaved float64
	var tExpectedMs, tHistoryCoveredMs, tPricedCoveredMs int64

	for i := days - 1; i >= 0; i-- {
		dayStart := todayMidnight.AddDate(0, 0, -i)
		dayKey := dayStart.Format("2006-01-02")
		isToday := i == 0

		var ds daySavings
		if isToday {
			b, err := s.deps.State.DailyCostBreakdownContext(r.Context(), dayStart.UnixMilli(), now.UnixMilli(), zone, ep)
			if err != nil {
				slog.Error("handleSavingsDaily: DailyCostBreakdown failed", "err", err, "day", dayKey)
				http.Error(w, "savings load failed", http.StatusInternalServerError)
				return
			}
			ds = fromBreakdown(b, resolutionFor(b))
		} else {
			s.savingsCacheMu.Lock()
			cached, ok := s.savingsCache[dayKey]
			s.savingsCacheMu.Unlock()
			if ok {
				ds = cached
			} else {
				dayEnd := dayStart.AddDate(0, 0, 1)
				b, err := s.deps.State.DailyCostBreakdownContext(r.Context(), dayStart.UnixMilli(), dayEnd.UnixMilli(), zone, ep)
				if err != nil {
					slog.Error("handleSavingsDaily: DailyCostBreakdown failed", "err", err, "day", dayKey)
					http.Error(w, "savings load failed", http.StatusInternalServerError)
					return
				}
				ds = fromBreakdown(b, resolutionFor(b))
				s.savingsCacheMu.Lock()
				s.savingsCache[dayKey] = ds
				s.savingsCacheMu.Unlock()
			}
		}

		tImpWh += ds.ImportWh
		tExpWh += ds.ExportWh
		tLoadWh += ds.LoadWh
		tActual += ds.ActualCostOre
		tBaseline += ds.BaselineCostOre
		tSaved += ds.SavedOre
		tExpectedMs += ds.ExpectedMs
		tHistoryCoveredMs += ds.HistoryCoveredMs
		tPricedCoveredMs += ds.PricedCoveredMs

		out = append(out, map[string]any{
			"day":                dayKey,
			"import_wh":          ds.ImportWh,
			"export_wh":          ds.ExportWh,
			"load_wh":            ds.LoadWh,
			"ev_wh":              ds.EVWh,
			"import_cost_ore":    ds.ImportCostOre,
			"export_revenue_ore": ds.ExportRevenueOre,
			"actual_cost_ore":    ds.ActualCostOre,
			"baseline_house_ore": ds.BaselineHouseOre,
			"baseline_ev_ore":    ds.BaselineEvOre,
			"baseline_cost_ore":  ds.BaselineCostOre,
			// Deprecated compatibility alias: now equals baseline_cost_ore
			// (house slot-priced + EV at daily-avg), not a flat-average tariff.
			"flat_cost_ore":        ds.FlatCostOre,
			"saved_ore":            ds.SavedOre,
			"avg_import_ore_kwh":   ds.AvgImportOreKwh,
			"avg_export_ore_kwh":   ds.AvgExportOreKwh,
			"expected_ms":          ds.ExpectedMs,
			"history_covered_ms":   ds.HistoryCoveredMs,
			"priced_covered_ms":    ds.PricedCoveredMs,
			"history_coverage_pct": ds.HistoryCoverage,
			"priced_coverage_pct":  ds.PricedCoverage,
			"resolution":           ds.Resolution,
		})
	}

	writeJSON(w, 200, map[string]any{
		"days": out,
		"totals": map[string]any{
			"import_wh":         tImpWh,
			"export_wh":         tExpWh,
			"load_wh":           tLoadWh,
			"actual_cost_ore":   tActual,
			"baseline_cost_ore": tBaseline,
			// Deprecated compatibility alias for older UI callers.
			"flat_cost_ore":        tBaseline,
			"saved_ore":            tSaved,
			"expected_ms":          tExpectedMs,
			"history_covered_ms":   tHistoryCoveredMs,
			"priced_covered_ms":    tPricedCoveredMs,
			"history_coverage_pct": boundedCoveragePct(tHistoryCoveredMs, tExpectedMs),
			"priced_coverage_pct":  boundedCoveragePct(tPricedCoveredMs, tExpectedMs),
		},
		"tz":          loc.String(),
		"value_scope": savingsValueScope,
		"baseline":    savingsBaseline,
	})
}

func boundedCoveragePct(coveredMs, expectedMs int64) float64 {
	if coveredMs <= 0 || expectedMs <= 0 {
		return 0
	}
	if coveredMs >= expectedMs {
		return 1
	}
	return float64(coveredMs) / float64(expectedMs)
}

// resolutionFor reports whether the breakdown saw any price data. A day
// with zero-price slots is still priced; only zero overlapping price slots
// means the UI should render "awaiting prices".
func resolutionFor(b state.DayCostBreakdown) string {
	if b.PriceSlotCount == 0 {
		return "no_prices"
	}
	return "slot"
}

// writeLedgerSavings answers from the energy ledger when it has rows in the
// window. An empty ledger leaves the caller on the older point-log path.
func (s *Server) writeLedgerSavings(w http.ResponseWriter, r *http.Request, days int, now time.Time, loc *time.Location, todayMidnight time.Time, zone string, ep state.ExportPricing) (bool, error) {
	windowStart := todayMidnight.AddDate(0, 0, -(days - 1))
	buckets, err := s.deps.State.LedgerFlowBuckets(r.Context(), windowStart.UnixMilli(), now.UnixMilli())
	if err != nil || len(buckets) == 0 {
		return false, err
	}
	rawSlots, err := s.deps.State.CostSlots(r.Context(), zone, windowStart.UnixMilli(), now.UnixMilli())
	if err != nil {
		return false, err
	}
	slots := make([]savings.PriceSlot, len(rawSlots))
	for i, sl := range rawSlots {
		slots[i] = savings.PriceSlot{StartMs: sl.StartMs, EndMs: sl.EndMs, ImportOreKwh: sl.ImportOreKwh, SpotOreKwh: sl.SpotOreKwh}
	}
	flows := make([]savings.FlowBucket, len(buckets))
	for i, b := range buckets {
		flows[i] = savings.FlowBucket{
			StartMs: b.StartMs, LenMs: b.LenMs,
			ImportWh: b.ImportWh, ExportWh: b.ExportWh, PVWh: b.PVWh, LoadWh: b.LoadWh,
			EVChargeWh: b.EVChargeWh, EVDischargeWh: b.EVDischargeWh,
		}
	}
	windows := make([]savings.DayWindow, days)
	keys := make([]string, days)
	for i := 0; i < days; i++ {
		start := todayMidnight.AddDate(0, 0, -(days - 1 - i))
		end := start.AddDate(0, 0, 1)
		if !end.Before(now) && !start.After(now) {
			end = now
		}
		windows[i] = savings.DayWindow{StartMs: start.UnixMilli(), EndMs: end.UnixMilli()}
		keys[i] = start.Format("2006-01-02")
	}
	bat, socDrivers := ledgerBattery(s.deps.Cfg)
	socPts := s.batterySOCSamples(r.Context(), socDrivers, windowStart.UnixMilli(), now.UnixMilli())
	if bat.CapacityWh > 0 {
		if wh, ok := socWhAt(socPts, windowStart.UnixMilli(), bat.CapacityWh); ok && wh > bat.StartWh {
			bat.StartWh = wh
		}
	}
	priced := savings.EvaluateLedger(flows, slots, windows, bat, ep)
	out := make([]map[string]any, 0, days)
	var tImp, tExp, tLoad, tActual, tBase, tSaved, tSelf, tSelfSaved float64
	var tExpected, tHist, tPrice int64
	for i, d := range priced {
		expected := windows[i].EndMs - windows[i].StartMs
		resolution := "no_prices"
		slotCount := 0
		var importSum, exportSum, importMs, exportMs float64
		for _, sl := range slots {
			if sl.EndMs <= windows[i].StartMs || sl.StartMs >= windows[i].EndMs {
				continue
			}
			slotCount++
			overlap := min64(sl.EndMs, windows[i].EndMs) - max64(sl.StartMs, windows[i].StartMs)
			importSum += sl.ImportOreKwh * float64(overlap)
			exportSum += sl.SpotOreKwh * float64(overlap)
			importMs += float64(overlap)
			exportMs += float64(overlap)
		}
		if slotCount > 0 {
			resolution = "slot"
		}
		selfSaved := d.SelfSavedOre()
		row := map[string]any{
			"day": keys[i], "import_wh": d.ImportWh, "export_wh": d.ExportWh,
			"load_wh": d.LoadWh, "ev_wh": d.EVWh, "pv_wh": d.PVWh,
			"import_cost_ore": d.ImportCostOre, "export_revenue_ore": d.ExportRevenueOre,
			"actual_cost_ore":    d.ImportCostOre - d.ExportRevenueOre,
			"baseline_house_ore": d.NoPVCostOre, "baseline_ev_ore": 0.0,
			"baseline_cost_ore": d.NoPVCostOre, "flat_cost_ore": d.NoPVCostOre,
			"saved_ore":                  d.NoPVSavedOre(),
			"self_consumption_cost_ore":  d.SelfCostOre,
			"self_consumption_saved_ore": selfSaved,
			"self_consumption_import_wh": d.SelfImportWh,
			"self_consumption_export_wh": d.SelfExportWh,
			"avg_import_ore_kwh":         divOrZero(importSum, importMs),
			"avg_export_ore_kwh":         divOrZero(exportSum, exportMs),
			"expected_ms":                expected,
			"history_covered_ms":         d.CoveredMs,
			"priced_covered_ms":          d.PricedMs,
			"history_coverage_pct":       boundedCoveragePct(d.CoveredMs, expected),
			"priced_coverage_pct":        boundedCoveragePct(d.PricedMs, expected),
			"resolution":                 resolution,
		}
		out = append(out, row)
		tImp += d.ImportWh
		tExp += d.ExportWh
		tLoad += d.LoadWh
		tActual += d.ImportCostOre - d.ExportRevenueOre
		tBase += d.NoPVCostOre
		tSaved += d.NoPVSavedOre()
		tSelf += d.SelfCostOre
		tSelfSaved += selfSaved
		tExpected += expected
		tHist += d.CoveredMs
		tPrice += d.PricedMs
	}
	writeJSON(w, 200, map[string]any{
		"days": out,
		"totals": map[string]any{
			"import_wh": tImp, "export_wh": tExp, "load_wh": tLoad,
			"actual_cost_ore": tActual, "baseline_cost_ore": tBase, "flat_cost_ore": tBase,
			"saved_ore":                 tSaved,
			"self_consumption_cost_ore": tSelf, "self_consumption_saved_ore": tSelfSaved,
			"expected_ms": tExpected, "history_covered_ms": tHist, "priced_covered_ms": tPrice,
			"history_coverage_pct": boundedCoveragePct(tHist, tExpected),
			"priced_coverage_pct":  boundedCoveragePct(tPrice, tExpected),
		},
		"tz": loc.String(), "value_scope": savingsValueScope, "baseline": savingsBaseline,
		"baselines": []string{"no_pv_no_battery", "self_consumption"},
	})
	return true, nil
}

func ledgerBattery(cfg *config.Config) (savings.Battery, []string) {
	if cfg == nil {
		return savings.Battery{}, nil
	}
	var capacity, chargeW, dischargeW float64
	var names []string
	for _, d := range cfg.Drivers {
		if d.BatteryCapacityWh <= 0 || d.BatteryTelemetryOnly {
			continue
		}
		capacity += d.BatteryCapacityWh
		c, dis := d.MaxChargeW, d.MaxDischargeW
		if c <= 0 {
			c = 5000
		}
		if dis <= 0 {
			dis = 5000
		}
		chargeW += c
		dischargeW += dis
		if d.Name != "" {
			names = append(names, d.Name)
		}
	}
	chg, disEff := dumbEfficiency(0), dumbEfficiency(0)
	if cfg.Planner != nil {
		if cfg.Planner.ChargeEfficiency > 0 {
			chg = dumbEfficiency(cfg.Planner.ChargeEfficiency)
		}
		if cfg.Planner.DischargeEfficiency > 0 {
			disEff = dumbEfficiency(cfg.Planner.DischargeEfficiency)
		}
	}
	reserve := capacity * dumbReserveFrac
	return savings.Battery{
		CapacityWh: capacity, MaxChargeW: chargeW, MaxDischargeW: dischargeW,
		StartWh: reserve, ReserveWh: reserve,
		ChargeEfficiency: chg, DischargeEfficiency: disEff,
	}, names
}

func dumbEfficiency(configured float64) float64 {
	if configured <= 0 || configured > 1 {
		configured = 0.95
	}
	v := configured - dumbEffHaircut
	if v < dumbEffFloor {
		return dumbEffFloor
	}
	return v
}

func (s *Server) batterySOCSamples(ctx context.Context, drivers []string, since, until int64) []state.SeriesPoint {
	if s == nil || s.deps.State == nil || len(drivers) == 0 || until <= since {
		return nil
	}
	var best []state.SeriesPoint
	for _, name := range drivers {
		// Coarse on purpose. A fine cap returns only the latest day, and the
		// stored-energy correction would then land on that day alone.
		span := until - since
		points := int(span/(2*time.Hour.Milliseconds())) + 2
		if points < 8 {
			points = 8
		}
		if points > 1500 {
			points = 1500
		}
		pts, err := s.deps.State.LoadSeriesBucketsContext(ctx, name, batterySOCMetric, since, until, points)
		if err != nil {
			slog.Warn("savings battery soc", "driver", name, "err", err)
			continue
		}
		if len(pts) > len(best) {
			best = pts
		}
	}
	return best
}

func socWhAt(points []state.SeriesPoint, at int64, capacityWh float64) (float64, bool) {
	frac, ok := socFractionAt(points, at)
	if !ok || capacityWh <= 0 {
		return 0, false
	}
	return frac * capacityWh, true
}

func socFractionAt(points []state.SeriesPoint, at int64) (float64, bool) {
	if len(points) == 0 {
		return 0, false
	}
	best := 0
	bestAbs := int64(1 << 62)
	for i, p := range points {
		d := p.TsMs - at
		if d < 0 {
			d = -d
		}
		if d < bestAbs {
			bestAbs = d
			best = i
		}
	}
	if bestAbs > 6*time.Hour.Milliseconds() {
		return 0, false
	}
	v := points[best].V
	if points[best].Last != nil {
		v = *points[best].Last
	}
	if v > 1.5 {
		v /= 100
	}
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v, true
}

func divOrZero(sum, weight float64) float64 {
	if weight <= 0 {
		return 0
	}
	return sum / weight
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *Server) ensureSavingsCache() {
	s.savingsCacheMu.Lock()
	defer s.savingsCacheMu.Unlock()
	if s.savingsCache == nil {
		s.savingsCache = make(map[string]daySavings)
	}
}
