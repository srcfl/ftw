package api

import "net/http"

// GET /api/heat/plan — each heat zone's plan from the last slot, what the
// zones together would save a day against the owner's least saving, and what
// was written. `write: false` means the plan is shadow and nothing was sent.
func (s *Server) handleHeatPlan(w http.ResponseWriter, r *http.Request) {
	if s.deps.HeatPlan == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "write": false})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.HeatPlan.Snapshot())
}
