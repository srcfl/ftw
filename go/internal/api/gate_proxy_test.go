package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
)

// TestGateBehindOwnerProxy reproduces the home.fortytwowatts.com path: an owner
// reverse-proxy stamps X-FTW-Tunnel (exactly like cmd/forty-two-watts/
// owner_relay_register.go) and forwards to the gated API. The gate MUST then
// treat the request as remote (NOT LAN-bypass) and refuse it without a session.
func TestGateBehindOwnerProxy(t *testing.T) {
	const marker = "test-tunnel-marker"
	deps := &Deps{TunnelMarker: marker, OwnerAccessLANBypass: true}
	gate := httptest.NewServer(New(deps).Handler())
	defer gate.Close()

	target, _ := url.Parse(gate.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	od := proxy.Director
	proxy.Director = func(req *http.Request) {
		od(req)
		req.Header.Set("X-FTW-Tunnel", marker)
	}
	front := httptest.NewServer(proxy)
	defer front.Close()

	resp, err := http.Get(front.URL + "/api/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 (gated remote), got %d", resp.StatusCode)
	}
}

// TestGateViaHostReconstruction replicates tunnel/host.go's handle() EXACTLY:
// rebuild the request from a TunneledRequest (browser headers, path only, raw
// map copy), then serve it through the owner proxy into a recorder. This is the
// untested link in the live home.* chain.
func TestGateViaHostReconstruction(t *testing.T) {
	const marker = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // 64 hex like the real marker
	deps := &Deps{TunnelMarker: marker, OwnerAccessLANBypass: true}
	gate := httptest.NewServer(New(deps).Handler())
	defer gate.Close()

	target, _ := url.Parse(gate.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	od := proxy.Director
	proxy.Director = func(req *http.Request) {
		od(req)
		req.Header.Set("X-FTW-Tunnel", marker)
	}

	// host.go: http.NewRequestWithContext(ctx, tr.Method, tr.Path, body); raw
	// header copy; handler.ServeHTTP(rec, inner).
	trHeader := http.Header{"User-Agent": {"Mozilla/5.0"}, "Accept": {"text/html"}}
	inner, _ := http.NewRequest("GET", "/api/status", bytes.NewReader(nil))
	for k, vv := range trHeader {
		inner.Header[k] = vv
	}
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, inner)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d (gate did not see the tunnel marker)", rec.Code)
	}
}
