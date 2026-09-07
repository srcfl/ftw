package api

import (
	"math"
	"net/http"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/configreload"
)

// handleLoadpointVehicle changes only the usual car's battery capacity. The
// runtime may still prefer a capacity reported by the car for this session.
func (s *Server) handleLoadpointVehicle(w http.ResponseWriter, r *http.Request) {
	if s.deps.Loadpoints == nil || s.deps.Cfg == nil || s.deps.CfgMu == nil || s.deps.SaveConfig == nil || s.deps.Ctrl == nil || s.deps.CtrlMu == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Charging settings are not available yet."})
		return
	}
	var body struct {
		CapacityWh float64 `json:"capacity_wh"`
	}
	if err := readJSON(r, &body); err != nil || math.IsNaN(body.CapacityWh) || math.IsInf(body.CapacityWh, 0) || body.CapacityWh < 1000 || body.CapacityWh > 300000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Enter a usable battery size from 1 to 300 kWh."})
		return
	}
	id := r.PathValue("id")
	s.configWriteMu.Lock()
	defer s.configWriteMu.Unlock()

	// Copy only the slice we edit. Hold the read lock through serialization so
	// another config writer cannot change referenced fields while they save.
	s.deps.CfgMu.RLock()
	next := *s.deps.Cfg
	next.Loadpoints = append([]config.Loadpoint(nil), s.deps.Cfg.Loadpoints...)
	index := -1
	for i := range next.Loadpoints {
		if next.Loadpoints[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		s.deps.CfgMu.RUnlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Charger not found in settings."})
		return
	}
	next.Loadpoints[index].VehicleCapacityWh = body.CapacityWh
	err := s.deps.SaveConfig(s.deps.ConfigPath, &next)
	s.deps.CfgMu.RUnlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Battery size could not be saved. The previous size is still in use."})
		return
	}
	configreload.Apply(s.deps.CfgMu, s.deps.Cfg, s.deps.CtrlMu, s.deps.Ctrl, &next, s.deps.ConfigApplier)
	if s.deps.ConfigApplier == nil {
		// Minimal embeddings do not wire main's shared callback. Change just
		// this capacity in the manager's existing configuration.
		points := s.deps.Loadpoints.Configs()
		for i := range points {
			if points[i].ID == id {
				points[i].VehicleCapacityWh = body.CapacityWh
			}
		}
		s.deps.Loadpoints.Load(points)
	}
	s.replanForScheduleChange(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vehicle_capacity_wh": body.CapacityWh, "capacity_source": "configured"})
}
