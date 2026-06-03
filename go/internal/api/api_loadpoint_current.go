package api

import "net/http"

// User-facing charge-current override — the dashboard amps slider (Tesla-style
// "set my charge current"). Distinct from the diagnostics manual_hold: it has
// no expiry, overrides surplus/schedule (the user asked to charge now), and is
// cleared automatically when the car unplugs. Lives in its own file per the
// api/CLAUDE.md split; registered via routes() in api.go.

type chargeCurrentRequest struct {
	Amps float64 `json:"amps"`
}

type chargeCurrentResponse struct {
	Amps float64 `json:"amps"`            // active override (0 = Auto/off)
	MaxA float64 `json:"max_a,omitempty"` // charger max, for the slider upper bound
}

// lpCurrentPreflight resolves + validates the loadpoint id, writing the error
// response itself. Returns (id, true) on success.
func (s *Server) lpCurrentPreflight(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.deps.LoadpointCtrl == nil {
		writeJSON(w, 503, map[string]string{"error": "loadpoint controller not available"})
		return "", false
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return "", false
	}
	if s.deps.Loadpoints == nil {
		writeJSON(w, 404, map[string]string{"error": "loadpoints not configured"})
		return "", false
	}
	if _, ok := s.deps.Loadpoints.State(id); !ok {
		writeJSON(w, 404, map[string]string{"error": "loadpoint not found"})
		return "", false
	}
	return id, true
}

// handleLoadpointChargeCurrent — POST /api/loadpoints/{id}/charge_current
// Body {"amps": N}. amps <= 0 clears the override (Auto). The override persists
// until cleared or the car unplugs.
func (s *Server) handleLoadpointChargeCurrent(w http.ResponseWriter, r *http.Request) {
	id, ok := s.lpCurrentPreflight(w, r)
	if !ok {
		return
	}
	var req chargeCurrentRequest
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.deps.LoadpointCtrl.SetManualCurrent(id, req.Amps)
	amps, maxA := s.deps.LoadpointCtrl.ManualCurrentInfo(id)
	writeJSON(w, 200, chargeCurrentResponse{Amps: amps, MaxA: maxA})
}

// handleLoadpointChargeCurrentClear — DELETE — back to Auto.
func (s *Server) handleLoadpointChargeCurrentClear(w http.ResponseWriter, r *http.Request) {
	id, ok := s.lpCurrentPreflight(w, r)
	if !ok {
		return
	}
	s.deps.LoadpointCtrl.ClearManualCurrent(id)
	_, maxA := s.deps.LoadpointCtrl.ManualCurrentInfo(id)
	writeJSON(w, 200, chargeCurrentResponse{Amps: 0, MaxA: maxA})
}

// handleLoadpointChargeCurrentGet — GET — active override + slider max.
func (s *Server) handleLoadpointChargeCurrentGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.lpCurrentPreflight(w, r)
	if !ok {
		return
	}
	amps, maxA := s.deps.LoadpointCtrl.ManualCurrentInfo(id)
	writeJSON(w, 200, chargeCurrentResponse{Amps: amps, MaxA: maxA})
}
