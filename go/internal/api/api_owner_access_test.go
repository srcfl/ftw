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

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
