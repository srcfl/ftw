package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
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
// no-PV/no-battery baseline. The endpoint name is kept for compatibility.
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
//	      "resolution": "slot" | "no_prices"
//	    },
//	    ...
//	  ],
//	  "totals": { "import_wh": ..., "export_wh": ..., "load_wh": ...,
//	              "actual_cost_ore": ..., "baseline_cost_ore": ..., "saved_ore": ... },
//	  "tz": "Local"
//	}
//
// Days where the prices table has no slot for the zone come back with
// resolution="no_prices" and zeroed costs. Volume columns are still
// populated for those days so the UI can distinguish "no data" from
// "data but no prices yet".
func (s *Server) handleSavingsDaily(w http.ResponseWriter, r *http.Request) {
	if s.deps.State == nil {
		writeJSON(w, 200, map[string]any{"days": []any{}})
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
		writeJSON(w, 200, map[string]any{"days": []any{}, "tz": time.Now().Location().String()})
		return
	}

	now := time.Now()
	loc := now.Location()
	todayMidnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)

	s.ensureSavingsCache()

	out := make([]map[string]any, 0, days)
	var tImpWh, tExpWh, tLoadWh, tActual, tBaseline, tSaved float64

	for i := days - 1; i >= 0; i-- {
		dayStart := todayMidnight.AddDate(0, 0, -i)
		dayKey := dayStart.Format("2006-01-02")
		isToday := i == 0

		var ds daySavings
		if isToday {
			b, err := s.deps.State.DailyCostBreakdown(dayStart.UnixMilli(), now.UnixMilli(), zone, ep)
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
				b, err := s.deps.State.DailyCostBreakdown(dayStart.UnixMilli(), dayEnd.UnixMilli(), zone, ep)
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
			"flat_cost_ore":      ds.FlatCostOre,
			"saved_ore":          ds.SavedOre,
			"avg_import_ore_kwh": ds.AvgImportOreKwh,
			"avg_export_ore_kwh": ds.AvgExportOreKwh,
			"resolution":         ds.Resolution,
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
			"flat_cost_ore": tBaseline,
			"saved_ore":     tSaved,
		},
		"tz": loc.String(),
	})
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

func (s *Server) ensureSavingsCache() {
	s.savingsCacheMu.Lock()
	defer s.savingsCacheMu.Unlock()
	if s.savingsCache == nil {
		s.savingsCache = make(map[string]daySavings)
	}
}
