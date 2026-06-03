package api

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
)

// minDeps assembles the smallest Deps that lets api.New run. Most
// handlers tested elsewhere need much more; owner-access only needs
// State + a parsed Cfg.
func minDeps(t *testing.T) *Deps {
	t.Helper()
	tmp := t.TempDir()
	st, err := state.Open(filepath.Join(tmp, "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tel := telemetry.NewStore()
	var capMu sync.RWMutex
	var cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	cfg := &config.Config{}
	cfg.Site.Name = "test-site"
	return &Deps{
		State:                st,
		Tel:                  tel,
		CapMu:                &capMu,
		Capacities:           map[string]float64{},
		CfgMu:                &cfgMu,
		Cfg:                  cfg,
		CtrlMu:               &ctrlMu,
		SaveConfig:           func(string, *config.Config) error { return nil },
		Restart:              func(context.Context) error { return nil },
		Version:              "test",
		OwnerAccessRPID:      "localhost",
		OwnerAccessOrigins:   []string{"http://localhost"},
		OwnerAccessLANBypass: true,
	}
}

func TestOwnerAccessWhoamiUnauthenticated(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = false
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/owner-access/whoami", nil)
	req.Host = "1.2.3.4" // not loopback
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestOwnerAccessWhoamiLANBypass(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/owner-access/whoami", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("LAN bypass should authenticate: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestOwnerAccessDevicesListEmpty(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/owner-access/devices", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"devices":[]`) && !contains(rec.Body.String(), `"devices":null`) {
		t.Fatalf("expected empty devices: %q", rec.Body.String())
	}
}

func TestOwnerAccessEnrollStartFirstDeviceAllowed(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = false
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("first enrollment should be allowed (bootstrap): status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"ceremony_token"`) {
		t.Fatalf("missing ceremony_token: %q", rec.Body.String())
	}
	if !contains(rec.Body.String(), `"publicKey"`) {
		t.Fatalf("missing webauthn options publicKey: %q", rec.Body.String())
	}
}

func TestOwnerAccessEnrollStartBlockedAfterFirstDevice(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = false
	// Pre-seed a device so enrollment requires auth.
	_ = d.State.SaveTrustedDevice(state.TrustedDevice{
		CredentialID: []byte("seed"), PublicKey: []byte("k"),
		FriendlyName: "seed", CreatedAtMs: time.Now().UnixMilli(),
	})
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("expected 403 without auth: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestOwnerAccessLoginStartRequiresEnrolledDevice(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/login/start", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("expected 404 with no devices enrolled: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// A relay-tunnelled request (carrying the trusted tunnel marker) must NOT
// inherit LAN-bypass even though it arrives at a loopback host. This is the
// single most important regression in the whole feature: without it every
// remote request silently skips the passkey.
func TestOwnerAccessTunneledRequestNeverBypasses(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true // bypass ON
	d.TunnelMarker = "test-marker-secret"
	srv := New(d)

	// Marked + loopback host + no cookie → must be treated as remote.
	req := httptest.NewRequest("GET", "/api/owner-access/devices", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "test-marker-secret")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("tunnelled request must require auth, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// An UNMARKED loopback/LAN request still bypasses when LANBypass is on.
func TestOwnerAccessUnmarkedRequestStillBypasses(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "test-marker-secret"
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/devices", nil)
	req.Host = "127.0.0.1:8080"
	// no X-FTW-Tunnel header
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("unmarked LAN request should bypass, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// A forged marker that doesn't match the per-process secret is NOT treated
// as a tunnel (constant-time compare); it just behaves like a normal LAN
// client (still bypassed) — never an escalation.
func TestOwnerAccessForgedMarkerIsNotTunnel(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "the-real-secret"
	srv := New(d)

	req := httptest.NewRequest("GET", "/api/owner-access/devices", nil)
	req.Host = "127.0.0.1:8080"
	req.Header.Set("X-FTW-Tunnel", "a-wrong-guess")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("wrong marker must behave as LAN (bypass), got %d", rec.Code)
	}
}

// First-enrollment (zero devices) is trust-on-first-use. Over the relay that
// window is internet-exposed, so a remote (marked) request must be refused —
// the first passkey must be enrolled on the LAN.
func TestOwnerAccessBootstrapBlockedOverTunnel(t *testing.T) {
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
		t.Fatalf("remote bootstrap must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// First-enrollment on the LAN (unmarked) is still allowed.
func TestOwnerAccessBootstrapAllowedOnLAN(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	req.Host = "127.0.0.1:8080" // unmarked → LAN
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("LAN bootstrap should be allowed, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// W is a stable opaque handle persisted in state.db — it must NOT change when
// the site is renamed (the whole point of decoupling owner identity from the
// mutable site name).
func TestOwnerWalletHandleStableAcrossRename(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	w1, err := srv.ownerWalletHandle()
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(w1) == 0 {
		t.Fatal("empty wallet handle")
	}
	// Simulate a site rename.
	d.Cfg.Site.Name = "renamed-site"
	w2, err := srv.ownerWalletHandle()
	if err != nil {
		t.Fatalf("handle 2: %v", err)
	}
	if string(w1) != string(w2) {
		t.Fatalf("wallet handle changed on rename: %q -> %q", w1, w2)
	}
	// And the WebAuthn owner id is the handle, not the site name.
	u, err := srv.buildOwnerUser()
	if err != nil {
		t.Fatalf("buildOwnerUser: %v", err)
	}
	if string(u.WebAuthnID()) != string(w2) {
		t.Fatalf("owner WebAuthnID = %q, want wallet handle %q", u.WebAuthnID(), w2)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
