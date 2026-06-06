package api

import (
	"context"
	"encoding/base64"
	"net/http"
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
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("LAN bypass should authenticate: status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestOwnerAccessLogoutRevokesSession(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = false // force the cookie/session path, not LAN bypass
	srv := New(d)

	// Issue a session and capture its cookie.
	issueRec := httptest.NewRecorder()
	if err := srv.issueOwnerSession(issueRec, []byte("cred-1")); err != nil {
		t.Fatalf("issue session: %v", err)
	}
	cookies := issueRec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != ownerAccessCookieName || cookies[0].Value == "" {
		t.Fatalf("expected a %s session cookie, got %+v", ownerAccessCookieName, cookies)
	}
	cookie := cookies[0]

	serve := func(method, path string, c *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Host = "1.2.3.4" // not loopback
		if c != nil {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// The session authenticates before logout.
	if rec := serve("GET", "/api/owner-access/whoami", cookie); rec.Code != 200 {
		t.Fatalf("session should authenticate before logout: status=%d", rec.Code)
	}

	// Logout returns 200 and expires the cookie.
	out := serve("POST", "/api/owner-access/logout", cookie)
	if out.Code != 200 {
		t.Fatalf("logout status=%d body=%q", out.Code, out.Body.String())
	}
	cleared := false
	for _, c := range out.Result().Cookies() {
		if c.Name == ownerAccessCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("logout did not expire the %s cookie", ownerAccessCookieName)
	}

	// The same cookie is now revoked server-side.
	if rec := serve("GET", "/api/owner-access/whoami", cookie); rec.Code != 401 {
		t.Errorf("session should be revoked after logout: status=%d", rec.Code)
	}
}

func TestEnrollPinBurnsAfterMaxTries(t *testing.T) {
	srv := New(minDeps(t))
	oa := srv.ownerAccess()
	pin, _, _, err := oa.mintEnrollPin()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// A guaranteed-wrong guess (first digit + 1 mod 10).
	wrong := string('0'+(pin[0]-'0'+1)%10) + pin[1:]

	for i := 0; i < ownerAccessEnrollPinMaxTries; i++ {
		if oa.validateEnrollPin(wrong) {
			t.Fatalf("wrong PIN accepted on attempt %d", i)
		}
	}
	// The PIN is now burned — even the correct one must be rejected.
	if oa.validateEnrollPin(pin) {
		t.Error("correct PIN should be rejected after the attempt cap (PIN burned)")
	}
}

func TestDeviceDeleteRevokesSessions(t *testing.T) {
	srv := New(minDeps(t)) // LAN bypass on → the DELETE is authorized from loopback
	oa := srv.ownerAccess()
	credID := []byte("cred-X")

	rec := httptest.NewRecorder()
	if err := srv.issueOwnerSession(rec, credID); err != nil {
		t.Fatalf("issue session: %v", err)
	}
	tok := rec.Result().Cookies()[0].Value
	oa.mu.Lock()
	_, exists := oa.authSessions[tok]
	oa.mu.Unlock()
	if !exists {
		t.Fatal("session was not created")
	}

	credB64 := base64.RawURLEncoding.EncodeToString(credID)
	del := httptest.NewRequest("DELETE", "/api/owner-access/devices/"+credB64, nil)
	del.Host = "127.0.0.1:8080"
	del.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source → LAN bypass authorizes
	delRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(delRec, del)
	if delRec.Code != 204 {
		t.Fatalf("delete status=%d body=%q", delRec.Code, delRec.Body.String())
	}

	oa.mu.Lock()
	_, still := oa.authSessions[tok]
	oa.mu.Unlock()
	if still {
		t.Error("session for the deleted credential should be revoked")
	}
}

func TestOwnerAccessDevicesListEmpty(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	req := httptest.NewRequest("GET", "/api/owner-access/devices", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
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
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
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
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
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
	req.Host = "127.0.0.1:8080"          // unmarked → LAN
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
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

// whoami reports the stable wallet handle so the browser can key on the
// wallet rather than the mutable site name.
func TestOwnerWhoamiReturnsWallet(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	w, _ := srv.ownerWalletHandle()
	req := httptest.NewRequest("GET", "/api/owner-access/whoami", nil)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "192.168.1.50:1234" // genuine private-range LAN source
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !contains(rec.Body.String(), `"wallet":"`+string(w)+`"`) {
		t.Fatalf("whoami missing wallet handle %q: %q", w, rec.Body.String())
	}
}

// The discoverable-login handler resolves the single owner from the
// assertion's userHandle (== the wallet handle W), and rejects any other.
func TestResolveDiscoverableOwner(t *testing.T) {
	d := minDeps(t)
	srv := New(d)
	w, _ := srv.ownerWalletHandle()
	u, err := srv.resolveDiscoverableOwner([]byte("rawid"), w)
	if err != nil {
		t.Fatalf("resolve with correct handle: %v", err)
	}
	if string(u.WebAuthnID()) != string(w) {
		t.Fatalf("resolved wrong user: %q", u.WebAuthnID())
	}
	if _, err := srv.resolveDiscoverableOwner([]byte("rawid"), []byte("not-the-wallet")); err == nil {
		t.Fatal("expected error for unknown wallet handle")
	}
}

// login/start must be discoverable: 200 with NO allowCredentials leaking the
// enrolled credential id (BeginLogin would include it; BeginDiscoverableLogin
// must not). 404 stays when nothing is enrolled.
func TestLoginStartIsDiscoverable(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	srv := New(d)
	if err := d.State.SaveTrustedDevice(state.TrustedDevice{
		CredentialID: []byte("seed"), PublicKey: []byte("k"), FriendlyName: "x",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/owner-access/login/start", nil)
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	// base64url("seed") == "c2VlZA" — must NOT appear in allowCredentials.
	if contains(rec.Body.String(), "c2VlZA") {
		t.Fatalf("allowCredentials leaked credential id — not discoverable: %q", rec.Body.String())
	}
}

// An owner session must survive a process restart (persisted to state.db) so a
// Pi reboot doesn't sign the operator out.
func TestOwnerSessionSurvivesRestart(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = false
	srv := New(d)
	rec := httptest.NewRecorder()
	if err := srv.issueOwnerSession(rec, []byte("cred")); err != nil {
		t.Fatalf("issue: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session cookie issued")
	}
	// Simulate a restart: drop in-memory state, fresh Server over the same db.
	d.ownerAccess = nil
	srv2 := New(d)
	req := httptest.NewRequest("GET", "/api/owner-access/whoami", nil)
	req.Host = "1.2.3.4"
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec2 := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("session must survive restart, got %d body=%q", rec2.Code, rec2.Body.String())
	}
}

// Post-bootstrap (an owner passkey already exists), a friend pair-flow request
// (loopback source, unmarked) must NOT be able to manage owner credentials —
// no enrolling its own passkey (permanent-owner escalation), no listing or
// deleting the owner's passkeys (recon / lockout). A genuine private-LAN owner
// still can. This is the fix for the Codex P1 friend->owner escalation.
func TestFriendLoopbackCannotManageOwner(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "marker"
	if err := d.State.SaveTrustedDevice(state.TrustedDevice{
		CredentialID: []byte("owner-cred"), PublicKey: []byte("k"),
		FriendlyName: "owner phone", CreatedAtMs: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("seed owner device: %v", err)
	}
	srv := New(d)

	send := func(remoteAddr, method, path string) int {
		req := httptest.NewRequest(method, path, nil)
		req.Host = "127.0.0.1:8080"
		req.RemoteAddr = remoteAddr // no X-FTW-Tunnel marker (friend-flow is unmarked)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	const friend = "127.0.0.1:55555"    // ftw-pair / relay reverse-proxy origin
	const lanOwner = "192.168.1.50:1234" // genuine private-range LAN owner
	credB64 := base64.RawURLEncoding.EncodeToString([]byte("owner-cred"))

	// Friend (loopback) is refused every owner-credential action.
	if code := send(friend, "POST", "/api/owner-access/enroll/start"); code != 403 {
		t.Errorf("friend enroll/start: got %d, want 403", code)
	}
	if code := send(friend, "GET", "/api/owner-access/devices"); code != 401 {
		t.Errorf("friend devices list: got %d, want 401", code)
	}
	if code := send(friend, "DELETE", "/api/owner-access/devices/"+credB64); code != 401 {
		t.Errorf("friend device delete: got %d, want 401", code)
	}

	// Genuine private-LAN owner can still list devices (manage path open to LAN).
	if code := send(lanOwner, "GET", "/api/owner-access/devices"); code != 200 {
		t.Errorf("LAN owner devices list: got %d, want 200", code)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
