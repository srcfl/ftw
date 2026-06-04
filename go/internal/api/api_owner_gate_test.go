package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// gateDeps = minDeps + a WebDir containing the passkey landing, so the gate's
// HTML fallback has something to serve.
func gateDeps(t *testing.T) *Deps {
	t.Helper()
	d := minDeps(t)
	web := t.TempDir()
	if err := os.MkdirAll(filepath.Join(web, "owner-access"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "owner-access", "index.html"),
		[]byte("<!doctype html><title>passkey login</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"),
		[]byte("<!doctype html><title>DASHBOARD</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.WebDir = web
	d.TunnelMarker = "marker"
	return d
}

// Remote (marked) + unauthenticated + API call → 401, never reaches the handler.
func TestGateBlocksRemoteAPI(t *testing.T) {
	d := gateDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/health", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("remote API must be 401, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// Remote (marked) + unauthenticated + HTML navigation → serves the passkey
// landing (200), NOT the dashboard.
func TestGateServesLoginForRemoteHTML(t *testing.T) {
	d := gateDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 302 {
		t.Fatalf("expected 302 redirect to login, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/owner-access/" {
		t.Fatalf("expected redirect to /owner-access/, got %q", loc)
	}
}

// Local (unmarked) request reaches the dashboard via bypass.
func TestGateAllowsLocalDashboard(t *testing.T) {
	d := gateDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !contains(rec.Body.String(), "DASHBOARD") {
		t.Fatalf("local must see dashboard, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// The login ceremony stays reachable even when remote + unauthenticated.
func TestGateLoginSurfaceStaysOpen(t *testing.T) {
	d := gateDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/login/start", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// 404 (no devices enrolled) proves the gate let it through to the handler.
	if rec.Code == 401 {
		t.Fatalf("login/start must not be gated; got 401")
	}
}

// Static assets (CSS/JS/images) must stay public for an unauthenticated remote
// request, so the login page renders styled. Only /api/* and the dashboard
// shell are gated.
func TestGateStaticAssetsPublicForUnauth(t *testing.T) {
	d := gateDeps(t)
	if err := os.WriteFile(filepath.Join(d.WebDir, "style.css"), []byte("body{color:#000}"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.OwnerAccessLANBypass = true
	srv := New(d)

	// (a) a static asset — unauth remote → served, NOT 401.
	req := httptest.NewRequest("GET", "/style.css", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("static asset must be public for unauth remote, got %d", rec.Code)
	}

	// (b) /api/* — unauth remote → still 401.
	req2 := httptest.NewRequest("GET", "/api/status", nil)
	req2.Host = "127.0.0.1:8080"
	req2.Header.Set("X-FTW-Tunnel", "marker")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != 401 {
		t.Fatalf("/api/* must stay gated for unauth remote, got %d", rec2.Code)
	}
}
