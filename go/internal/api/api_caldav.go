package api

import "net/http"

// handleCalDAVStatus renders the calendar-constraints client's diagnostic
// snapshot (issue #498): whether it's enabled, whether the CalDAV server is
// reachable, the last sync time, the parsed-intent counts, the live away
// state, the next EV deadline, and the subscribe URL + username the UI shows
// the operator to paste into their calendar app.
//
// Mirrors handleHAStatus: pure read, no mutation, nil-safe. When the feature
// is disabled (Deps.CalDAV nil) it reports {enabled:false} rather than 503 so
// the Settings tab can render a clean "disabled in config" state.
func (s *Server) handleCalDAVStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.CalDAV == nil {
		writeJSON(w, 200, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, 200, s.deps.CalDAV.Status())
}
