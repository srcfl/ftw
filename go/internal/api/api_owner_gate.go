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
		// On-host liveness exception: deploy/CI smoke-tests and the docker
		// HEALTHCHECK curl http://127.0.0.1/api/health on loopback and must not
		// be gated. Restricted to a loopback SOURCE that did NOT arrive via the
		// relay tunnel — the relay proxy also connects from loopback but stamps
		// the marker, so a remote visitor can never reach this path. The probe
		// returns only coarse up/down counts, never control or secrets.
		if isLoopbackLivenessPath(r.URL.Path) && isLoopbackSource(r) && !s.isTunneled(r) {
			next.ServeHTTP(w, r)
			return
		}
		// Unauthenticated remote. The data + control surface (/api/*) is never
		// served without a session. Static assets (CSS/JS/images) stay public
		// so the login page itself renders styled; only the dashboard's
		// app-shell HTML routes redirect an unauthenticated visitor to the
		// passkey login.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && acceptsHTML(r) && isDashboardShell(r.URL.Path) {
			s.serveOwnerLogin(w, r)
			return
		}
		next.ServeHTTP(w, r)
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
		"/api/owner-access/whoami",
		"/api/owner-access/logout",
		// C3 device-key PoP login: these are the silent-login analogue of
		// login/start + login/finish — the device key IS the credential, so they
		// must be reachable pre-session over the P2P channel. They mint a session
		// only on a valid PoP against a PINNED trusted device; an unauthenticated
		// caller learns nothing (challenge is a random nonce) and can mint nothing
		// without a key the Pi already trusts. This does NOT relax the gate for any
		// other /api/* path.
		"/api/owner-access/device-challenge",
		"/api/owner-access/device-pop",
		// /api/identity is the TOFU anchor for the P2P-only home route: the
		// browser MUST fetch + pin the Pi's PUBLIC ES256 key before it can open
		// (and verify) a P2P channel, so this must be reachable unauthenticated
		// over the relay. It returns only the public key + site_id + curve — no
		// secret, no control surface — so exposing it costs nothing and is the
		// same security posture as the public login surface and static assets.
		"/api/identity":
		return true
	}
	return strings.HasPrefix(p, "/owner-access/")
}

func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// isLoopbackLivenessPath lists the non-sensitive liveness probes that on-host
// healthchecks (deploy smoke-tests, docker HEALTHCHECK, CI) reach over the
// loopback interface. Kept deliberately tiny: only coarse up/down signals with
// no control surface or secrets may appear here, since the gate grants them to
// any loopback-source request that is not relay-tunnelled.
func isLoopbackLivenessPath(p string) bool {
	return p == "/api/health"
}

// isDashboardShell reports whether the path is one of the dashboard's HTML
// entry points (the SPA shell). An unauthenticated remote visitor hitting one
// is redirected to the passkey login; all other static files stay public so
// the login surface renders styled.
func isDashboardShell(p string) bool {
	switch p {
	case "/", "/index.html", "/setup", "/setup.html", "/legacy", "/legacy.html", "/next":
		return true
	}
	return false
}

// serveOwnerLogin redirects an unauthenticated dashboard-shell hit to the
// owner-access landing page (sign in OR enroll), served at its real path so
// the page's relative module imports (./webauthn.js) resolve.
func (s *Server) serveOwnerLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/owner-access/", http.StatusFound)
}
