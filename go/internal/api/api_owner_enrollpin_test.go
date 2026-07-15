package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
)

// The LAN PIN is the bridge that lets the owner enroll the FIRST passkey at
// the relay.ftw.sourceful.energy origin (required for the relay RP-ID) while
// proving LAN presence via a code only a local user can read.

func minEnrollPinDeps(t *testing.T) (*Deps, *fakeBootstrapRelay) {
	t.Helper()
	d := minDeps(t)
	d.Cfg.RemoteAccess = &config.RemoteAccess{Enabled: true}
	signer := newFakeInstanceSigner(t)
	d.InstanceSigner = signer
	d.SiteIdentityPubHex = signer.PublicKeyHex()
	d.SiteID = "site:test-site"
	fr, relaySrv := newFakeBootstrapRelay(t)
	d.RelayBaseURL = relaySrv.URL
	return d, fr
}

// enroll-pin must return 200 + a 6-digit pin on a genuine (non-tunnelled) LAN
// request after Remote Access is opted in, and 403 on a relay-tunnelled
// (marked) request.
func TestEnrollPinLANvsTunnel(t *testing.T) {
	d, fr := minEnrollPinDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)

	// Genuine LAN (unmarked) → 200 with a pin.
	req := httptest.NewRequest("GET", "/api/owner-access/enroll-pin", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("LAN enroll-pin should be 200, got %d body=%q", rec.Code, rec.Body.String())
	}
	var out struct {
		Pin         string `json:"pin"`
		BootstrapID string `json:"bootstrap_id"`
		ExpiresIn   int    `json:"expires_in_s"`
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
	// The response MUST carry a high-entropy bootstrap_id (>=32 bytes CSPRNG,
	// base64url-no-pad) — the relay's unguessable claim handle. It must NOT be
	// derivable from the 6-digit PIN.
	bid, err := base64.RawURLEncoding.DecodeString(out.BootstrapID)
	if err != nil {
		t.Fatalf("bootstrap_id is not base64url-no-pad: %q (%v)", out.BootstrapID, err)
	}
	if len(bid) < 32 {
		t.Fatalf("bootstrap_id must be >=32 bytes of entropy, got %d", len(bid))
	}
	fr.mu.Lock()
	gotPut := fr.gotPut
	fr.mu.Unlock()
	if !gotPut {
		t.Fatalf("enroll-pin returned before parking the setup descriptor on the relay")
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
	if got := rec2.Header().Get("X-FTW-Error-Code"); got != errEnrollPinLANOnly {
		t.Fatalf("tunnelled enroll-pin error code = %q, want %q", got, errEnrollPinLANOnly)
	}
}

// mintEnrollPin issues a pin over a genuine LAN request and returns it.
func mintEnrollPin(t *testing.T, srv *Server) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/owner-access/enroll-pin", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
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

func TestOwnerEnrollPinRequiresRemoteAccessOptIn(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/owner-access/enroll-pin", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.168.1.50:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("remote-off enroll-pin should be 409, got %d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-FTW-Error-Code"); got != errRemoteAccessOff {
		t.Fatalf("remote-off error code = %q, want %q", got, errRemoteAccessOff)
	}
}

func TestOwnerEnrollPinRequiresRestartAfterOptIn(t *testing.T) {
	d := minDeps(t)
	d.Cfg.RemoteAccess = &config.RemoteAccess{Enabled: true}
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	d.RelayBaseURL = "" // cfg was hot-toggled, but service has not restarted.
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/owner-access/enroll-pin", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.168.1.50:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Fatalf("restart-needed enroll-pin should be 409, got %d body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-FTW-Error-Code"); got != errRemoteRestartRequired {
		t.Fatalf("restart-needed error code = %q, want %q", got, errRemoteRestartRequired)
	}
}

// A tunnelled bootstrap enroll/start WITHOUT a pin must be 403.
func TestEnrollPinTunnelBootstrapWithoutPin(t *testing.T) {
	d, _ := minEnrollPinDeps(t)
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
	if got := rec.Header().Get("X-FTW-Error-Code"); got != errEnrollPinRequired {
		t.Fatalf("missing-pin error code = %q, want %q", got, errEnrollPinRequired)
	}
}

// A tunnelled bootstrap enroll/start WITH the correct minted pin must succeed
// (ceremony begins → ceremony_token present).
func TestEnrollPinTunnelBootstrapWithPin(t *testing.T) {
	d, _ := minEnrollPinDeps(t)
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
	d, _ := minEnrollPinDeps(t)
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
	if got := rec.Header().Get("X-FTW-Error-Code"); got != errEnrollPinRequired {
		t.Fatalf("wrong-pin error code = %q, want %q", got, errEnrollPinRequired)
	}
}

// A friend-flow request reaches the Pi via the ftw-pair sidecar from loopback,
// unmarked (it carries no X-FTW-Tunnel marker — only the owner long-poll proxy
// stamps that). On an un-enrolled Pi such a request must NOT be able to
// bootstrap-enroll itself as the owner: the PIN-less LAN bootstrap requires a
// genuine private-range LAN source, which loopback (the relay/sidecar proxy) is
// not. This is what stops a "friend" from hijacking owner enrollment.
func TestOwnerAccessBootstrapRefusedFromLoopbackProxy(t *testing.T) {
	d, _ := minEnrollPinDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "127.0.0.1:55555" // loopback: the ftw-pair / relay proxy source
	// deliberately no X-FTW-Tunnel marker — a friend-flow request is unmarked
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("loopback-proxy bootstrap must be refused, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// The matching guard for the PIN: a loopback-proxy (friend-flow) request must
// not be able to mint the enrollment PIN either.
func TestOwnerEnrollPinRefusedFromLoopbackProxy(t *testing.T) {
	d, _ := minEnrollPinDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/owner-access/enroll-pin", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "127.0.0.1:55555" // loopback proxy source, unmarked
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("loopback-proxy enroll-pin must be refused, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// LAN (unmarked) bootstrap enroll/start still works without any pin — the PIN
// requirement is only for the internet-exposed tunnel path.
func TestEnrollPinLANBootstrapNoPinStillWorks(t *testing.T) {
	d, _ := minEnrollPinDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	srv := New(d)
	req := httptest.NewRequest("POST", "/api/owner-access/enroll/start", nil)
	req.Host = "127.0.0.1:8080"          // unmarked → LAN
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("LAN bootstrap should be allowed without pin, got %d body=%q", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"ceremony_token"`) {
		t.Fatalf("missing ceremony_token: %q", rec.Body.String())
	}
}
