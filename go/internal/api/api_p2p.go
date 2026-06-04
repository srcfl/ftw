package api

// api_p2p.go — the Pi side of the browser P2P signaling handshake. The browser
// POSTs its WebRTC SDP offer here; because this route arrives over the owner
// tunnel (X-FTW-Tunnel set) it is owner-gated like the rest of /api/*, so only
// an authenticated owner can stand up a direct DataChannel. The Pi answers and
// the resulting DTLS DataChannel carries tunnel frames straight to the local
// API (see go/internal/p2p). Signaling reuses the authenticated tunnel — no
// relay changes.

import (
	"log/slog"
	"net/http"
)

// handleP2POffer accepts a browser SDP offer and returns the Pi's SDP answer.
// Opening a DataChannel is owner-credential-grade: it grants a direct
// browser↔Pi tunnel that OUTLIVES a pair grant (until the channel's own GC), so
// it must use the STRICT authorizer (a real session or a genuine private-range
// LAN source, never the loopback bypass). Otherwise a friend pair-flow request
// (loopback, unmarked) could stand up a channel and keep owner access after the
// grant expires.
func (s *Server) handleP2POffer(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorizeOwnerManage(r); !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.deps.P2P == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "p2p unavailable"})
		return
	}
	var req struct {
		Type string `json:"type"`
		SDP  string `json:"sdp"`
	}
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.SDP == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing sdp"})
		return
	}
	// Capture the offer's auth context so the DataChannel's replayed requests
	// carry the SAME trust tier the owner has here: forward the session cookie,
	// and stamp the tunnel marker only when this offer itself arrived tunnelled
	// (remote). On the LAN the marker is absent, preserving LAN-bypass; over the
	// relay it forces the remote tier so DataChannel requests get no LAN-only
	// escalation (see p2p.Bridge).
	replay := http.Header{}
	if ck := r.Header.Get("Cookie"); ck != "" {
		replay.Set("Cookie", ck)
	}
	if s.isTunneled(r) {
		replay.Set("X-FTW-Tunnel", s.deps.TunnelMarker)
	}
	answerSDP, err := s.deps.P2P.Answer(r.Context(), req.SDP, replay)
	if err != nil {
		slog.Warn("p2p: answer failed", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "p2p handshake failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"type": "answer", "sdp": answerSDP})
}
