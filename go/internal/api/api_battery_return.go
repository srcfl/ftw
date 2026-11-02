package api

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/srcfl/ftw/go/internal/batteryreturn"
)

// Skatteverket refunds energy tax on battery-stored grid energy from 2018.
const batteryReturnFirstYear = 2018

// handleBatteryReturn answers how much grid energy the battery sent back to
// the grid in one Swedish calendar year: the figure for Skatteverket's refund
// of energy tax after battery storage.
func (s *Server) handleBatteryReturn(w http.ResponseWriter, r *http.Request) {
	rep, status, err := s.batteryReturn(r)
	if err != nil {
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	months := make([]map[string]any, len(rep.Months))
	for i, m := range rep.Months {
		months[i] = map[string]any{
			"month": i + 1, "returned_wh": m.Returned, "hours": m.Hours, "complete_hours": m.CompleteHours,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"year":           rep.Year,
		"zone":           batteryreturn.Zone,
		"returned_wh":    rep.Returned,
		"hours":          len(rep.Hours),
		"complete_hours": rep.CompleteHours(),
		"efficiency":     rep.Efficiency,
		"capacity_wh":    rep.CapacityWh,
		"batteries":      rep.Batteries,
		"problem":        rep.Problem,
		"months":         months,
	})
}

// handleBatteryReturnCSV is every hour behind the figure, for the owner to
// keep with the application.
func (s *Server) handleBatteryReturnCSV(w http.ResponseWriter, r *http.Request) {
	rep, status, err := s.batteryReturn(r)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="ftw-battery-return-%d.csv"`, rep.Year))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"hour_start", "grid_import_wh", "grid_export_wh", "battery_charge_wh", "battery_discharge_wh",
		"solar_wh", "vehicle_discharge_wh", "grid_to_battery_wh", "battery_to_grid_wh",
		"returned_wh", "grid_held_wh", "other_held_wh", "complete", "efficiency", "capacity_wh",
	})
	wh := func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
	for _, h := range rep.Hours {
		_ = cw.Write([]string{
			h.Start.Format(time.RFC3339), wh(h.GridImport), wh(h.GridExport), wh(h.BatteryCharge),
			wh(h.BatteryDischarge), wh(h.Solar), wh(h.VehicleDischarge), wh(h.GridToBattery),
			wh(h.BatteryToGrid), wh(h.Returned), wh(h.GridHeld), wh(h.OtherHeld), strconv.FormatBool(h.Complete),
			strconv.FormatFloat(h.Efficiency, 'f', 4, 64), wh(rep.CapacityWh),
		})
	}
	cw.Flush()
}

func (s *Server) batteryReturn(r *http.Request) (batteryreturn.Report, int, error) {
	if s.deps.State == nil {
		return batteryreturn.Report{}, http.StatusServiceUnavailable, fmt.Errorf("energy history unavailable")
	}
	loc, err := time.LoadLocation(batteryreturn.Zone)
	if err != nil {
		return batteryreturn.Report{}, http.StatusInternalServerError, err
	}
	now := time.Now()
	thisYear := now.In(loc).Year()
	// The year an owner applies for is the one that has ended.
	year := thisYear - 1
	if raw := r.URL.Query().Get("year"); raw != "" {
		year, err = strconv.Atoi(raw)
		if err != nil || year < batteryReturnFirstYear || year > thisYear {
			return batteryreturn.Report{}, http.StatusBadRequest,
				fmt.Errorf("year must be %d to %d", batteryReturnFirstYear, thisYear)
		}
	}
	var site batteryreturn.Site
	if s.deps.Cfg != nil && s.deps.CfgMu != nil {
		s.deps.CfgMu.RLock()
		site = batteryreturn.SiteOf(s.deps.Cfg)
		s.deps.CfgMu.RUnlock()
	}
	rep, err := batteryreturn.Build(r.Context(), s.deps.State, year, now, site)
	if err != nil {
		return batteryreturn.Report{}, http.StatusInternalServerError, err
	}
	return rep, http.StatusOK, nil
}
