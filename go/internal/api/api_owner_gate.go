// api_owner_gate.go
//
// Global authentication gate for the owner-access remote path. Wraps the
// entire mux: the passkey login surface is always reachable; everything
// else (the dashboard at "/" and every other /api/*) requires an authorized
// owner. Genuine LAN/loopback requests pass via authorizeOwner's LAN-bypass;
// relay-tunnelled (remote) requests are excluded from bypass (see
// isTunneled) and must carry a valid ftw_owner session.
package api

import (
	"net/http"
	"path/filepath"
	"strings"
)

// gate wraps next with the owner auth-gate. Returned by Server.Handler().
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The gate exists to protect the relay/tunnel (remote) path. When
		// tunnel detection is disabled (no relay wired — TunnelMarker empty),
		// there are no remote requests to identify, so the gate has nothing
		// to enforce and defers to the pre-existing LAN-trust model. The main
		// binary always sets TunnelMarker (cmd/forty-two-watts/main.go), so
		// the gate is always active in production; only minimal test Deps omit
		// it. This is the same TunnelMarker-gated condition as isTunneled.
		if s.deps.TunnelMarker == "" {
			next.ServeHTTP(w, r)
			return
		}
		if isOwnerAccessOpenPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := s.authorizeOwner(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		// Unauthenticated and remote (bypass already declined inside
		// authorizeOwner for tunnelled requests). Serve the passkey landing
		// for top-level navigations; 401 for API/asset calls.
		if r.Method == http.MethodGet && acceptsHTML(r) {
			s.serveOwnerLogin(w, r)
			return
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// isOwnerAccessOpenPath lists the paths reachable without an authorized
// session — the passkey login surface and its assets. enroll/* is listed
// here but is independently gated by enrollAllowed (incl. bootstrap
// hardening for remote requests). Paths are what the Pi sees: the relay
// strips its /me/<site_id> prefix before forwarding.
func isOwnerAccessOpenPath(p string) bool {
	switch p {
	case "/api/owner-access/enroll-pin",
		"/api/owner-access/login/start",
		"/api/owner-access/login/finish",
		"/api/owner-access/enroll/start",
		"/api/owner-access/enroll/finish",
		"/api/owner-access/whoami":
		return true
	}
	return strings.HasPrefix(p, "/owner-access/")
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// serveOwnerLogin serves the passkey landing page without leaking the
// dashboard. Uses a file serve (no Location header) so it works regardless
// of the relay's /me/<site_id> prefix.
func (s *Server) serveOwnerLogin(w http.ResponseWriter, r *http.Request) {
	landing := filepath.Clean(filepath.Join(s.deps.WebDir, "owner-access", "index.html"))
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	http.ServeFile(w, r, landing)
}
