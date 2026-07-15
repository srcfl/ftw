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
// Static, non-sensitive metadata — no mutex, no owner state. The UI fetches it
// plainly (it's safe to serve over the relay); commands still go through the
// strict /api/mode path.
func (s *Server) handleModes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"modes": control.ModeCatalog()})
}
