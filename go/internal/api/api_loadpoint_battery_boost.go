package api

import (
	"context"
	"net/http"
	"time"

	"github.com/srcfl/ftw/go/internal/loadpoint"
)

type batteryBoostRequest struct {
	DurationS        int64   `json:"duration_s,omitempty"`
	ExpiresAtMs      int64   `json:"expires_at_ms,omitempty"`
	MinBatterySoCPct float64 `json:"min_battery_soc_pct"`
	EVTargetSoCPct   float64 `json:"ev_target_soc_pct,omitempty"`
	DepartureAtMs    int64   `json:"departure_at_ms,omitempty"`
}

func (s *Server) loadpointForBatteryBoost(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.deps.LoadpointCtrl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "loadpoint controller not available"})
		return "", false
	}
	if s.deps.Loadpoints == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "loadpoints not configured"})
		return "", false
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return "", false
	}
	if _, ok := s.deps.Loadpoints.State(id); !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "loadpoint not found"})
		return "", false
	}
	return id, true
}

// POST /api/loadpoints/{id}/battery_boost creates one absolute, bounded lease.
// Exactly one of duration_s and expires_at_ms is required. Core safety is
// checked before installation and again on every loadpoint dispatch tick.
func (s *Server) handleLoadpointBatteryBoostEnable(w http.ResponseWriter, r *http.Request) {
	id, ok := s.loadpointForBatteryBoost(w, r)
	if !ok {
		return
	}
	var req batteryBoostRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if (req.DurationS > 0) == (req.ExpiresAtMs > 0) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "set exactly one of duration_s or expires_at_ms"})
		return
	}
	now := time.Now()
	expires := time.UnixMilli(req.ExpiresAtMs)
	if req.DurationS > 0 {
		expires = now.Add(time.Duration(req.DurationS) * time.Second)
	}
	lease := loadpoint.BatteryBoostLease{
		StartedAt:        now,
		ExpiresAt:        expires,
		MinBatterySoCPct: req.MinBatterySoCPct,
		EVTargetSoCPct:   req.EVTargetSoCPct,
	}
	if req.DepartureAtMs > 0 {
		lease.DepartureAt = time.UnixMilli(req.DepartureAtMs)
	}
	if err := loadpoint.ValidateBatteryBoostLease(lease, now); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	status, err := s.deps.LoadpointCtrl.EnableBatteryBoost(id, lease, now)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	if s.deps.MPC != nil {
		go s.deps.MPC.ReplanWithReason(context.Background(), "loadpoint_battery_boost_enabled")
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleLoadpointBatteryBoostCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.loadpointForBatteryBoost(w, r)
	if !ok {
		return
	}
	status := s.deps.LoadpointCtrl.CancelBatteryBoost(id, time.Now())
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleLoadpointBatteryBoostStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := s.loadpointForBatteryBoost(w, r)
	if !ok {
		return
	}
	_, status := s.deps.LoadpointCtrl.BatteryBoost(id, time.Now())
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) decorateLoadpointsWithBatteryBoost(states []loadpoint.State) {
	if s.deps.LoadpointCtrl == nil {
		for i := range states {
			states[i].BatteryBoost = loadpoint.BatteryBoostStatus{State: "inactive"}
		}
		return
	}
	now := time.Now()
	for i := range states {
		_, states[i].BatteryBoost = s.deps.LoadpointCtrl.BatteryBoost(states[i].ID, now)
	}
}
