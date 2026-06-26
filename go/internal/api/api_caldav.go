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

// handleCalDAVCredentials reveals the managed Radicale credential (username +
// password) plus subscribe URLs so the Settings → Calendar tab can show them
// (and render a QR) for the operator to add the account to a phone/desktop
// calendar app. It intentionally returns the password — the owner needs it —
// and is gated by owner-access auth like the rest of /api/* (kept separate from
// the frequently-polled /status so the secret isn't read on every poll).
func (s *Server) handleCalDAVCredentials(w http.ResponseWriter, r *http.Request) {
	if s.deps.CalDAV == nil {
		writeJSON(w, 200, map[string]any{"managed": false})
		return
	}
	writeJSON(w, 200, s.deps.CalDAV.Credentials())
}
