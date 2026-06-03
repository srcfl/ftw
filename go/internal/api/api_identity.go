// api_identity.go — read-only surface for the Pi's self-sovereign ES256
// identity (the same key Nova reuses when federation is enabled).
package api

import "net/http"

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if s.deps.SiteIdentityPubHex == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "site identity unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"public_key_hex": s.deps.SiteIdentityPubHex,
		"algorithm":      "ES256",
		"curve":          "P-256",
	})
}
