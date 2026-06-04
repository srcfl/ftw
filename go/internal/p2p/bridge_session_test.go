package p2p

import (
	"net/http"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// bridge_session_test.go — slice-5: the Bridge captures the ftw_owner session
// from a login-finish-style response over the channel and stamps it as a Cookie
// on every subsequent frame, so the owner session lives only inside DTLS.

// TestBridge_CapturesAndStampsSession proves the channel-scoped session: a
// handler that sets Set-Cookie: ftw_owner=<tok> on /login causes the Bridge to
// stamp Cookie: ftw_owner=<tok> on the NEXT frame.
func TestBridge_CapturesAndStampsSession(t *testing.T) {
	const tok = "session-token-xyz"
	var seenCookie string
	handler := http.NewServeMux()
	handler.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ftw_owner", Value: tok, Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	handler.HandleFunc("/after", func(w http.ResponseWriter, r *http.Request) {
		seenCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	})

	// Marker-only auth context (the fail-closed signaling path), no cookie.
	auth := http.Header{}
	auth.Set("X-FTW-Tunnel", "marker")
	br := NewReplayer(handler, auth)

	// Before login the Bridge has no session, so /after sees no owner cookie.
	br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: "/after"})
	if seenCookie != "" {
		t.Fatalf("pre-login frame carried a cookie = %q, want none", seenCookie)
	}

	// Login over the channel mints the cookie; the Bridge captures it.
	br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: "/login"})
	if got := br.session(); got != tok {
		t.Fatalf("captured session = %q, want %q", got, tok)
	}

	// The next frame is stamped with the captured cookie.
	br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: "/after"})
	if seenCookie != "ftw_owner="+tok {
		t.Fatalf("post-login frame Cookie = %q, want ftw_owner=%s", seenCookie, tok)
	}
}

// TestBridge_RejectsClientSuppliedSessionCookie proves a browser cannot forge a
// session by smuggling ftw_owner in the frame's own headers: the Bridge strips
// any client-supplied ftw_owner before replay (only a captured login-finish may
// set it). Other cookies survive.
func TestBridge_RejectsClientSuppliedSessionCookie(t *testing.T) {
	var seenCookie string
	handler := http.NewServeMux()
	handler.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {
		seenCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	})
	auth := http.Header{}
	auth.Set("X-FTW-Tunnel", "marker")
	br := NewReplayer(handler, auth)

	frame := tunnel.TunneledRequest{
		Method: http.MethodGet,
		Path:   "/x",
		Header: http.Header{"Cookie": {"ftw_owner=FORGED; other=keep"}},
	}
	br.Replay(frame)
	if seenCookie != "other=keep" {
		t.Fatalf("client-forged ftw_owner survived: Cookie = %q, want only other=keep", seenCookie)
	}
}

// TestBridge_RejectsClientSuppliedMarker proves a client cannot supply the
// tunnel marker — only the trusted auth context may. (Pre-existing invariant,
// re-asserted under the new cookie-stripping code path.)
func TestBridge_RejectsClientSuppliedMarker(t *testing.T) {
	var seenMarker string
	handler := http.NewServeMux()
	handler.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) {
		seenMarker = r.Header.Get("X-FTW-Tunnel")
		w.WriteHeader(http.StatusOK)
	})
	// Auth context stamps the REAL marker; the client tries to override it.
	auth := http.Header{}
	auth.Set("X-FTW-Tunnel", "real-marker")
	br := NewReplayer(handler, auth)

	frame := tunnel.TunneledRequest{
		Method: http.MethodGet,
		Path:   "/x",
		Header: http.Header{"X-Ftw-Tunnel": {"client-forged"}},
	}
	br.Replay(frame)
	if seenMarker != "real-marker" {
		t.Fatalf("marker = %q, want real-marker (client value must be dropped)", seenMarker)
	}
}

// TestBridge_StripsSetCookieFromResponse is the FIX-5 guard: a login-finish
// response over the channel sets ftw_owner via Set-Cookie, which the Bridge
// captures internally — but the response that crosses the DataChannel must carry
// NO Set-Cookie, so the owner session can never be read by JS. The capture is
// proven by a later frame being stamped with the cookie server-side.
func TestBridge_StripsSetCookieFromResponse(t *testing.T) {
	const tok = "secret-session-tok"
	var seenCookie string
	handler := http.NewServeMux()
	handler.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		// Emit BOTH the owner cookie and an unrelated one, to prove ALL Set-Cookie
		// is dropped from the crossing response (not just ftw_owner).
		http.SetCookie(w, &http.Cookie{Name: "ftw_owner", Value: tok, Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "unrelated", Value: "x", Path: "/"})
		w.WriteHeader(http.StatusOK)
	})
	handler.HandleFunc("/after", func(w http.ResponseWriter, r *http.Request) {
		seenCookie = r.Header.Get("Cookie")
		w.WriteHeader(http.StatusOK)
	})

	auth := http.Header{}
	auth.Set("X-FTW-Tunnel", "marker")
	br := NewReplayer(handler, auth)

	resp := br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: "/login"})
	// The crossing response must carry NO Set-Cookie at all.
	if got := resp.Header.Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("response leaked Set-Cookie over the channel: %v (FIX-5: must be stripped)", got)
	}
	// But the session WAS captured internally — a later frame is stamped with it.
	if br.session() != tok {
		t.Fatalf("session not captured (FIX-5 must strip the crossing header WITHOUT breaking capture): %q", br.session())
	}
	br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: "/after"})
	if seenCookie != "ftw_owner="+tok {
		t.Fatalf("post-login frame Cookie = %q, want ftw_owner=%s", seenCookie, tok)
	}
}

// TestBridge_LogoutClearsSession proves a logout over the channel (a clearing
// ftw_owner cookie) drops the captured session so later frames are unauthorized.
func TestBridge_LogoutClearsSession(t *testing.T) {
	handler := http.NewServeMux()
	handler.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ftw_owner", Value: "tok", Path: "/"})
	})
	handler.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "ftw_owner", Value: "", Path: "/", MaxAge: -1})
	})
	auth := http.Header{}
	auth.Set("X-FTW-Tunnel", "marker")
	br := NewReplayer(handler, auth)

	br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: "/login"})
	if br.session() == "" {
		t.Fatal("session not captured after login")
	}
	br.Replay(tunnel.TunneledRequest{Method: http.MethodGet, Path: "/logout"})
	if br.session() != "" {
		t.Fatalf("session = %q after logout, want cleared", br.session())
	}
}
