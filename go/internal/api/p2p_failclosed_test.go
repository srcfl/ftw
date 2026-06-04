package api

import (
	"net/http"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/p2p"
	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// p2p_failclosed_test.go — the slice-5 regression guard for the P2P-only home
// route. With signaling off the tunnel, the DataChannel starts UNAUTHENTICATED:
// the offer carries no owner cookie. The Pi's signaling-path Answer stamps the
// tunnel marker on the Bridge's replay headers so every frame is REMOTE and can
// NEVER inherit lan-bypass. The browser then logs in OVER the channel; the
// Bridge captures the resulting ftw_owner Set-Cookie and stamps it on later
// frames. These tests prove both halves.

// gatedSignalServer builds a real gated api.Server (TunnelMarker set) wrapped so
// a side-door path can mint an owner session exactly the way login-finish does
// (emitting Set-Cookie: ftw_owner=<tok>), without a full WebAuthn ceremony. The
// Bridge replays against this combined handler — the SAME gated handler
// production wires into p2pMgr.SetLocalAPI.
func gatedSignalServer(t *testing.T) (http.Handler, *Server) {
	t.Helper()
	d := minDeps(t)
	d.TunnelMarker = "test-marker"
	// LAN-bypass ON is the dangerous case: it MUST NOT fire for a marked P2P
	// frame. If the marker stamping is wrong, this is what would fail open.
	d.OwnerAccessLANBypass = true
	srv := New(d)
	gated := srv.Handler()

	mux := http.NewServeMux()
	// Side door: stand in for login/finish — mint a real session + Set-Cookie.
	// Reachable only in-process (this test handler), never registered in prod.
	mux.HandleFunc("/__login_finish", func(w http.ResponseWriter, r *http.Request) {
		if err := srv.issueOwnerSession(w, []byte("p2p-failclosed-cred")); err != nil {
			t.Errorf("issue session: %v", err)
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", gated)
	return mux, srv
}

// marked is the fail-closed replay context the Pi's signaling-path Answer
// stamps: the tunnel marker only, NO owner cookie. This is what makes a
// session-less P2P frame REMOTE so the gate denies it.
func marked() http.Header {
	h := http.Header{}
	h.Set("X-FTW-Tunnel", "test-marker")
	return h
}

// TestP2PFrame_NoSession_Gets401 is invariant #1: a Bridge-replayed frame with
// the remote marker stamped but NO captured session is 401 on owner-gated paths.
// It must NEVER inherit lan-bypass even though LAN-bypass is enabled.
func TestP2PFrame_NoSession_Gets401(t *testing.T) {
	handler, _ := gatedSignalServer(t)
	br := p2p.NewReplayer(handler, marked())

	for _, path := range []string{"/api/status", "/api/owner-access/devices"} {
		resp := br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: path})
		if resp.Status != http.StatusUnauthorized {
			t.Errorf("session-less P2P frame %s: status = %d, want 401 (must not inherit lan-bypass)",
				path, resp.Status)
		}
	}
}

// TestP2PFrame_AfterLogin_Gets200 is invariant #2: once the browser logs in over
// the channel (the Bridge captures ftw_owner from the login-finish Set-Cookie),
// subsequent owner-gated frames are authorized.
func TestP2PFrame_AfterLogin_Gets200(t *testing.T) {
	handler, _ := gatedSignalServer(t)
	br := p2p.NewReplayer(handler, marked())

	// Use an owner-gated endpoint that only needs State (minDeps provides it):
	// /api/owner-access/devices is 401 without a session and 200 with one. The
	// gate decision — not the endpoint's payload — is what this invariant proves.
	const ownerPath = "/api/owner-access/devices"

	// 1. Pre-login: owner path is denied.
	if resp := br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: ownerPath}); resp.Status != http.StatusUnauthorized {
		t.Fatalf("pre-login %s = %d, want 401", ownerPath, resp.Status)
	}

	// 2. Login over the channel: the side-door login-finish emits
	//    Set-Cookie: ftw_owner=<tok>, which the Bridge captures.
	login := br.Replay(tunnel.TunneledRequest{Method: http.MethodPost, Path: "/__login_finish"})
	if login.Status != http.StatusOK {
		t.Fatalf("login-finish over channel = %d, want 200", login.Status)
	}
	if !hasOwnerSetCookie(login.Header) {
		t.Fatalf("login-finish response did not carry a ftw_owner Set-Cookie (capture would be impossible)")
	}

	// 3. Post-login: the SAME channel now authorizes owner frames, because the
	//    Bridge stamps the captured ftw_owner cookie on every later frame.
	if resp := br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: ownerPath}); resp.Status != http.StatusOK {
		t.Fatalf("post-login %s = %d, want 200 (captured channel session)", ownerPath, resp.Status)
	}
}

func hasOwnerSetCookie(h http.Header) bool {
	for _, sc := range h.Values("Set-Cookie") {
		if len(sc) >= len("ftw_owner=") && sc[:len("ftw_owner=")] == "ftw_owner=" {
			return true
		}
	}
	return false
}
