package api

import (
	"net/http"

	"github.com/srcfl/ftw/go/internal/thermaltwin"
)

// GET /api/thermal/twins — each declared room sensor's learned twin, in
// shadow. `shadow: true` is part of the answer, not decoration: nothing in
// Core plans or commands from these, and a reader must not infer that a
// trusted twin is steering anything.
func (s *Server) handleThermalTwins(w http.ResponseWriter, r *http.Request) {
	twins := []thermaltwin.ZoneStatus{}
	if s.deps.ThermalTwin != nil {
		twins = s.deps.ThermalTwin.Snapshot()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shadow":  true,
		"enabled": s.deps.ThermalTwin != nil,
		"twins":   twins,
	})
}
