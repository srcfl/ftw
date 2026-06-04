package api

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// The LAN PIN is the bridge that lets the owner enroll the FIRST passkey at
// the relay.fortytwowatts.com origin (required for the relay RP-ID) while
// proving LAN presence via a code only a local user can read.

// enroll-pin must return 200 + a 6-digit pin on a genuine (non-tunnelled) LAN
// request, and 403 on a relay-tunnelled (marked) request.
func TestEnrollPinLANvsTunnel(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)

	// Genuine LAN (unmarked) → 200 with a pin.
	req := httptest.NewRequest("GET", "/api/owner-access/enroll-pin", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("LAN enroll-pin should be 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var out struct {
		Pin       string `json:"pin"`
		ExpiresIn int    `json:"expires_in_s"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode pin response: %v body=%q", err, rec.Body.String())
	}
	if len(out.Pin) != 6 {
		t.Fatalf("expected 6-digit pin, got %q", out.Pin)
	}
	for _, c := range out.Pin {
		if c < '0' || c > '9' {
			t.Fatalf("pin must be numeric, got %q", out.Pin)
		}
	}
	if out.ExpiresIn <= 0 || out.ExpiresIn > 600 {
		t.Fatalf("expected expires_in_s in (0,600], got %d", out.ExpiresIn)
	}

	// Relay-tunnelled (marked) → 403, never hand out the PIN over the tunnel.
	req2 := httptest.NewRequest("GET", "/api/owner-access/enroll-pin", nil)
	req2.Host = "127.0.0.1:8080"
	req2.Header.Set("X-FTW-Tunnel", "marker")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != 403 {
		t.Fatalf("tunnelled enroll-pin must be 403, got %d body=%q", rec2.Code, rec2.Body.String())
	}
}

// mintEnrollPin issues a pin over a genuine LAN request and returns it.
func mintEnrollPin(t *testing.T, srv *Server) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/owner-access/enroll-pin", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("mint pin: status=%d body=%q", rec.Code, rec.Body.String())
	}
	var out struct {
		Pin string `json:"pin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("mint pin decode: %v", err)
	}
	return out.Pin
}

// A tunnelled bootstrap enroll/start WITHOUT a pin must be 403.
func TestEnrollPinTunnelBootstrapWithoutPin(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("tunnelled bootstrap without pin must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// A tunnelled bootstrap enroll/start WITH the correct minted pin must succeed
// (ceremony begins → ceremony_token present).
func TestEnrollPinTunnelBootstrapWithPin(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)

	pin := mintEnrollPin(t, srv)

	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start?pin="+pin, nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tunnelled bootstrap with correct pin should be 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"ceremony_token"`) {
		t.Fatalf("missing ceremony_token: %q", rec.Body.String())
	}
}

// A tunnelled bootstrap enroll/start with a WRONG pin must be 403.
func TestEnrollPinTunnelBootstrapWrongPin(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)

	_ = mintEnrollPin(t, srv) // a real pin exists, but we send the wrong one

	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start?pin=000000", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("tunnelled bootstrap with wrong pin must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// LAN (unmarked) bootstrap enroll/start still works without any pin — the PIN
// requirement is only for the internet-exposed tunnel path.
func TestEnrollPinLANBootstrapNoPinStillWorks(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	req.Host = "127.0.0.1:8080" // unmarked → LAN
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("LAN bootstrap should be allowed without pin, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"ceremony_token"`) {
		t.Fatalf("missing ceremony_token: %q", rec.Body.String())
	}
}
