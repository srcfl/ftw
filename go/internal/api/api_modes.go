package api

import (
	"net/http"

	"github.com/srcfl/ftw/go/internal/control"
)

// handleModes serves the operator-facing mode catalog: every selectable mode
// with its label, tooltip, and dashboard tier. The web UI builds its mode
// buttons from this list instead of hard-coding them, so the dashboard, the
// /api/mode validator, and the Home Assistant discovery `select` all derive
// from the same canonical control mode set and can't drift apart.
//
// Static, non-sensitive metadata; commands use the separate /api/mode path.
func (s *Server) handleModes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"modes": control.ModeCatalog()})
}
