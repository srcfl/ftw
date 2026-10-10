package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

// GET /api/ev/energy
//
// Energy and cash cost of EV charging for the last 7 and last 30 local days,
// including today. Days are civil midnights in the box's timezone, the same
// rolling windows as /api/energy/daily?days=N, not a calendar week or a
// month-to-date total.
//
// Cost uses the stored import price (total_ore_kwh). See state.EVAttributionNote.
// cost_partial is true when some grid-attributed energy had no price slot;
// energy_wh still includes that energy.
func (s *Server) handleEVEnergy(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	loc := now.Location()
	currency := "SEK"
	zone := ""
	labels := map[string]string{}
	if s.deps.CfgMu != nil && s.deps.Cfg != nil {
		s.deps.CfgMu.RLock()
		if s.deps.Cfg.Price != nil {
			zone = s.deps.Cfg.Price.Zone
			if s.deps.Cfg.Price.Currency != "" {
				currency = s.deps.Cfg.Price.Currency
			}
		}
		for _, lp := range s.deps.Cfg.Loadpoints {
			if lp.DriverName != "" && lp.ID != "" {
				labels[lp.DriverName] = lp.ID
			}
		}
		s.deps.CfgMu.RUnlock()
	}

	body := map[string]any{
		"tz":               loc.String(),
		"currency":         currency,
		"window":           "rolling_local_days",
		"attribution_note": state.EVAttributionNote,
		"windows":          []any{},
	}
	if s.deps.State == nil {
		writeJSON(w, http.StatusOK, body)
		return
	}

	days30 := state.LocalDays(now, 30)
	days7 := days30[len(days30)-7:]
	since := days30[0].StartMs
	until := now.UnixMilli()
	samples, err := s.deps.State.LoadEVChargeSamples(r.Context(), since, until)
	if err != nil {
		slog.Error("handleEVEnergy: load samples", "err", err)
		http.Error(w, "ev energy load failed", http.StatusInternalServerError)
		return
	}
	site, err := s.deps.State.LedgerFlowBuckets(r.Context(), since, until)
	if err != nil {
		slog.Error("handleEVEnergy: load site flows", "err", err)
		http.Error(w, "ev energy load failed", http.StatusInternalServerError)
		return
	}
	var slots []state.CostSlot
	if zone != "" {
		slots, err = s.deps.State.CostSlots(r.Context(), zone, since, until)
		if err != nil {
			slog.Error("handleEVEnergy: load prices", "err", err)
			http.Error(w, "ev energy load failed", http.StatusInternalServerError)
			return
		}
	}
	week := state.AggregateEVCharging(samples, site, slots, days7)
	month := state.AggregateEVCharging(samples, site, slots, days30)
	body["windows"] = []any{
		evEnergyWindowBody("7d", "Last 7 days", week, labels),
		evEnergyWindowBody("30d", "Last 30 days", month, labels),
	}
	writeJSON(w, http.StatusOK, body)
}

func evEnergyWindowBody(id, label string, w state.EVEnergyWindow, loadpoints map[string]string) map[string]any {
	daily := make([]any, len(w.Daily))
	for i, day := range w.Daily {
		daily[i] = map[string]any{
			"day":          day.Day,
			"energy_wh":    day.Total.EnergyWh,
			"grid_wh":      day.Total.GridWh,
			"onsite_wh":    day.Total.OnsiteWh,
			"cost_ore":     day.Total.CostOre,
			"unpriced_wh":  day.Total.UnpricedWh,
			"cost_partial": day.Total.CostPartial(),
			"chargers":     evChargerBodies(day.Chargers, loadpoints),
		}
	}
	return map[string]any{
		"id":           id,
		"label":        label,
		"days":         w.Days,
		"since_ms":     w.SinceMs,
		"until_ms":     w.UntilMs,
		"attribution":  w.Attribution,
		"energy_wh":    w.Total.EnergyWh,
		"grid_wh":      w.Total.GridWh,
		"onsite_wh":    w.Total.OnsiteWh,
		"cost_ore":     w.Total.CostOre,
		"unpriced_wh":  w.Total.UnpricedWh,
		"cost_partial": w.Total.CostPartial(),
		"chargers":     evChargerBodies(w.Chargers, loadpoints),
		"daily":        daily,
	}
}

func evChargerBodies(chargers []state.EVChargerCost, loadpoints map[string]string) []any {
	out := make([]any, 0, len(chargers))
	for _, c := range chargers {
		out = append(out, map[string]any{
			"asset_id":     c.AssetID,
			"label":        c.Label,
			"loadpoint_id": loadpoints[c.Label],
			"energy_wh":    c.EnergyWh,
			"grid_wh":      c.GridWh,
			"onsite_wh":    c.OnsiteWh,
			"cost_ore":     c.CostOre,
			"unpriced_wh":  c.UnpricedWh,
			"cost_partial": c.CostPartial(),
		})
	}
	return out
}
