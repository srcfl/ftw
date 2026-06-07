package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// home_web_test.go — single-tenant SLICE 1 (serve the home shell from
// -home-web), multi-tenant remote loader/static routing, SLICE 2 (answer
// /api/identity from the pinned home pubkey), and C1 (/me/register publishes
// the site's trusted device keys).

// TestMeRegister_PublishesDeviceKeys (C1) proves a device_pubkeys array on the
// ES256-signed /me/register is stored per-site, canonicalised + de-duped, and
// malformed entries are dropped without rejecting the registration.
func TestMeRegister_PublishesDeviceKeys(t *testing.T) {
	relay := newSignedRelay()
	relay.Polls = NewPollSecrets()
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	id := newTestIdentity(t)
	d1 := newDeviceKey(t)
	d2 := newDeviceKey(t)
	tsMs := time.Now().UnixMilli()
	keys := []string{
		d1.pubKeyHex,
		d2.pubKeyHex,
		d1.pubKeyHex, // duplicate — must be de-duped, not double-stored
		"not-a-key",  // malformed — must be dropped, not reject the whole reg
	}
	// v2 signing string covers the device_pubkeys set (so it can't be tampered).
	sig, err := id.SignRawHex(tunnel.MeRegisterSigningStringV2("site:Home", "host-owner", tsMs, keys))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body, _ := json.Marshal(meRegisterRequest{
		SiteID: "site:Home", HostID: "host-owner",
		PublicKey: id.PublicKeyHex(), TsMs: tsMs, Sig: sig,
		DevicePubkeys: keys,
	})
	resp, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status=%d body=%q (a malformed device key must not reject the reg)", resp.StatusCode, b)
	}
	if !relay.Owners.HasDeviceKey("site:Home", d1.pubKeyHex) {
		t.Fatal("device key 1 was not published")
	}
	if !relay.Owners.HasDeviceKey("site:Home", d2.pubKeyHex) {
		t.Fatal("device key 2 was not published")
	}
	stranger := newDeviceKey(t)
	if relay.Owners.HasDeviceKey("site:Home", stranger.pubKeyHex) {
		t.Fatal("a key that was never published must not be trusted")
	}
}

// TestMeRegister_DeviceKeys_ReplaceOnReRegister proves the Pi's set REPLACES (not
// merges): a key dropped on the Pi disappears from the relay on the next
// re-registration with the same site key.
func TestMeRegister_DeviceKeys_ReplaceOnReRegister(t *testing.T) {
	relay := newSignedRelay()
	relay.Polls = NewPollSecrets()
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	id := newTestIdentity(t)
	old := newDeviceKey(t)
	post := func(keys []string) {
		tsMs := time.Now().UnixMilli()
		sig, _ := id.SignRawHex(tunnel.MeRegisterSigningStringV2("site:Home", "host-owner", tsMs, keys))
		body, _ := json.Marshal(meRegisterRequest{
			SiteID: "site:Home", HostID: "host-owner",
			PublicKey: id.PublicKeyHex(), TsMs: tsMs, Sig: sig,
			DevicePubkeys: keys,
		})
		resp, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("register status=%d", resp.StatusCode)
		}
	}
	post([]string{old.pubKeyHex})
	if !relay.Owners.HasDeviceKey("site:Home", old.pubKeyHex) {
		t.Fatal("first device key not published")
	}
	// Re-register with a DIFFERENT set (the owner revoked `old`, added `now`).
	now := newDeviceKey(t)
	post([]string{now.pubKeyHex})
	if relay.Owners.HasDeviceKey("site:Home", old.pubKeyHex) {
		t.Fatal("revoked device key still trusted after re-register (set must REPLACE, not merge)")
	}
	if !relay.Owners.HasDeviceKey("site:Home", now.pubKeyHex) {
		t.Fatal("new device key not published on re-register")
	}
}

// TestMeRegister_BurnsBootstrapOnDeviceKeys (R3 / C1) proves a /me/register that
// publishes a NON-EMPTY device-key set burns any live bootstrap blob for that
// site: once a device is enrolled the first-enrollment window MUST close, so a
// stale/replayed claim_key can never reach an already-enrolled Pi. Defence in
// depth on top of the Pi's own enrollAllowed zero-device check.
func TestMeRegister_BurnsBootstrapOnDeviceKeys(t *testing.T) {
	relay := newMultiTenantRelay(t)
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	id := newTestIdentity(t)
	const site, hostID = "site:Home", "host-owner"
	claimKey := claimKeyForHW("bootstrap-secret-meregister")

	// Pin the site key first (a bare Register) so the bootstrap PUT path / Active
	// could resolve it, then park a live bootstrap window for the site.
	if err := relay.Owners.Register(site, hostID, id.PublicKeyHex()); err != nil {
		t.Fatalf("pre-register: %v", err)
	}
	if err := relay.Bootstrap.Put(site, []byte(`{"site":"site:Home"}`), claimKey, time.Minute); err != nil {
		t.Fatalf("bootstrap put: %v", err)
	}
	if !relay.Bootstrap.Live(site, claimKey) {
		t.Fatal("bootstrap not Live after Put; test precondition broken")
	}

	// A device gets enrolled: the Pi re-registers carrying its now-non-empty
	// trusted device-key set.
	dev := newDeviceKey(t)
	keys := []string{dev.pubKeyHex}
	tsMs := time.Now().UnixMilli()
	sig, err := id.SignRawHex(tunnel.MeRegisterSigningStringV2(site, hostID, tsMs, keys))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body, _ := json.Marshal(meRegisterRequest{
		SiteID: site, HostID: hostID,
		PublicKey: id.PublicKeyHex(), TsMs: tsMs, Sig: sig,
		DevicePubkeys: keys,
	})
	resp, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register status=%d want 200", resp.StatusCode)
	}

	// The bootstrap window must be gone now that a device is enrolled.
	if relay.Bootstrap.Live(site, claimKey) {
		t.Fatal("bootstrap still Live after /me/register published a device key; must be burned")
	}
	// And the enroll-forward gate is now closed for that claim_key: the live-blob
	// check fires (and 403s) before the home-reachability check, so a 403 here is
	// the burned window — no tunnel host needed.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/owner-access/enroll/start?claim_key="+claimKey+"&pin=123456", nil)
	req.Host = "home.test"
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusForbidden {
		t.Fatalf("enroll/start after device-key register: got %d, want 403 (burned)", r2.StatusCode)
	}
}

// TestMeRegister_EmptyDeviceKeysDoesNotBurn (R3) proves the burn is gated on a
// NON-EMPTY device-key set: a registration with no device keys (e.g. a Pi that
// hasn't enrolled anyone yet, re-registering for any reason) must NOT close an
// open bootstrap window — otherwise a routine re-registration during onboarding
// would lock the user out of their own first enrollment.
func TestMeRegister_EmptyDeviceKeysDoesNotBurn(t *testing.T) {
	relay := newMultiTenantRelay(t)
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	id := newTestIdentity(t)
	const site, hostID = "site:Home", "host-owner"
	claimKey := claimKeyForHW("bootstrap-secret-no-burn")
	if err := relay.Owners.Register(site, hostID, id.PublicKeyHex()); err != nil {
		t.Fatalf("pre-register: %v", err)
	}
	if err := relay.Bootstrap.Put(site, []byte(`{"site":"site:Home"}`), claimKey, time.Minute); err != nil {
		t.Fatalf("bootstrap put: %v", err)
	}

	// Re-register WITHOUT device keys (v1 signing string, empty set).
	tsMs := time.Now().UnixMilli()
	sig, err := id.SignRawHex(tunnel.MeRegisterSigningString(site, hostID, tsMs))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body, _ := json.Marshal(meRegisterRequest{
		SiteID: site, HostID: hostID,
		PublicKey: id.PublicKeyHex(), TsMs: tsMs, Sig: sig,
	})
	resp, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register status=%d want 200", resp.StatusCode)
	}
	if !relay.Bootstrap.Live(site, claimKey) {
		t.Fatal("bootstrap was burned by a register with NO device keys; window must stay open during onboarding")
	}
}

// claimKeyForHW mirrors the relay/browser claimKey = hex(sha256(bootstrap_id))
// for the home_web register tests (claimKeyHex lives in bootstrap_http_test.go;
// both compute the same digest).
func claimKeyForHW(bootstrapID string) string {
	h := sha256.Sum256([]byte(bootstrapID))
	return hex.EncodeToString(h[:])
}

// TestHomeIdentity_ServedFromPin (SLICE 2) proves GET /api/identity on the home
// host is answered from the pinned -home-pubkey WITHOUT forwarding to the Pi —
// and works even with no Pi registered at all.
func TestHomeIdentity_ServedFromPin(t *testing.T) {
	const pin = "1111111111111111111111111111111111111111111111111111111111111111" +
		"2222222222222222222222222222222222222222222222222222222222222222"
	relay := &Relay{
		Queue:      tunnel.NewQueue(),
		Tokens:     NewTokenRegistry(),
		Owners:     NewOwnerRegistry(),
		Polls:      NewPollSecrets(),
		Signals:    NewSignalMailbox(),
		Challenges: NewSignalChallenges(),
		HomeHost:   "home.test",
		HomeSite:   "site:Home",
		HomePubKey: pin,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/identity", nil)
	req.Host = "home.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("/api/identity from pin status=%d, want 200 (no Pi forward needed)", resp.StatusCode)
	}
	var out struct {
		PublicKeyHex string `json:"public_key_hex"`
		SiteID       string `json:"site_id"`
		Algorithm    string `json:"algorithm"`
		Curve        string `json:"curve"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if out.PublicKeyHex != pin {
		t.Fatalf("public_key_hex = %q, want the pinned key", out.PublicKeyHex)
	}
	if out.SiteID != "site:Home" || out.Algorithm != "ES256" || out.Curve != "P-256" {
		t.Fatalf("identity fields = %+v, want site:Home/ES256/P-256", out)
	}
}

// TestHomeStaticForward_ServesFromHomeWeb (SLICE 1) proves that with -home-web
// set, the home host's static GETs are served from the relay's disk (not
// forwarded to the Pi), "/" serves index.html, traversal is blocked, and an
// /api/ path is still refused.
func TestHomeStaticForward_ServesFromHomeWeb(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>SHELL</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret file OUTSIDE the web root, to prove traversal can't reach it.
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(secret) })

	relay := &Relay{
		Queue:      tunnel.NewQueue(),
		Tokens:     NewTokenRegistry(),
		Owners:     NewOwnerRegistry(),
		Polls:      NewPollSecrets(),
		Signals:    NewSignalMailbox(),
		Challenges: NewSignalChallenges(),
		HomeHost:   "home.test",
		HomeSite:   "site:Home",
		HomeWeb:    dir,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	get := func(path string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Host = "home.test"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	// "/" serves index.html from disk — no Pi registered, yet it works.
	if r, body := get("/"); r.StatusCode != 200 || body != "<h1>SHELL</h1>" {
		t.Fatalf(`GET "/" status=%d body=%q, want 200 "<h1>SHELL</h1>" from -home-web`, r.StatusCode, body)
	}
	// A named asset is served from disk.
	if r, body := get("/app.js"); r.StatusCode != 200 || body != "console.log(1)" {
		t.Fatalf("GET /app.js status=%d body=%q, want the on-disk file", r.StatusCode, body)
	}
	// A deep SPA route with no file falls back to index.html (SPA convention).
	if r, body := get("/dashboard/energy"); r.StatusCode != 200 || body != "<h1>SHELL</h1>" {
		t.Fatalf("GET /dashboard/energy status=%d body=%q, want the SPA shell", r.StatusCode, body)
	}
	// Path traversal must NOT escape the web root.
	if r, body := get("/../secret.txt"); body == "TOPSECRET" {
		t.Fatalf("path traversal leaked the secret file: status=%d body=%q", r.StatusCode, body)
	}
	// An /api/ path is still refused at the relay (P2P-only) even with -home-web.
	if r, _ := get("/api/owner-access/whoami"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("/api/* over relay status=%d, want 403 even with -home-web", r.StatusCode)
	}
}

// TestMeRegister_DeviceKeysTamperRejected locks in the #2 fix: a registration
// replayed with a swapped/added device_pubkeys array (not what the owner signed)
// must be REJECTED — the v2 signature covers the set, so an injected key is never
// trusted for signaling.
func TestMeRegister_DeviceKeysTamperRejected(t *testing.T) {
	relay := newSignedRelay()
	relay.Polls = NewPollSecrets()
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	id := newTestIdentity(t)
	good := newDeviceKey(t)
	attacker := newDeviceKey(t)
	tsMs := time.Now().UnixMilli()
	// Owner signs over the GOOD set; attacker replays with their key swapped in.
	sig, _ := id.SignRawHex(tunnel.MeRegisterSigningStringV2("site:Home", "host-owner", tsMs, []string{good.pubKeyHex}))
	body, _ := json.Marshal(meRegisterRequest{
		SiteID: "site:Home", HostID: "host-owner",
		PublicKey: id.PublicKeyHex(), TsMs: tsMs, Sig: sig,
		DevicePubkeys: []string{attacker.pubKeyHex},
	})
	resp, err := http.Post(srv.URL+"/me/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("tampered device_pubkeys must be rejected (the v2 signature covers the set)")
	}
	if relay.Owners.HasDeviceKey("site:Home", attacker.pubKeyHex) {
		t.Fatal("attacker key must NOT be trusted after a tampered register")
	}
}

// Under a SINGLE-TENANT pin (multi-tenant off), the wallet/identity API paths are
// not routes, but the home-host catch-all must still NOT serve or forward them:
// they are reserved to 404, while ordinary SPA paths still serve the shell.
func TestSingleTenantReservesWalletPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>SHELL</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	relay := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(), Owners: NewOwnerRegistry(),
		Polls: NewPollSecrets(), Signals: NewSignalMailbox(), Challenges: NewSignalChallenges(),
		HomeHost: "home.test", HomeSite: "site:Home", HomeWeb: dir, // single-tenant, MultiTenant=false
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()
	get := func(path string) int {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Host = "home.test"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/wallet/" + testHandle + "/blob"); code != http.StatusNotFound {
		t.Errorf("single-tenant /wallet = %d, want 404 (reserved, not SPA/forward)", code)
	}
	if code := get("/signal/site:Home/identity"); code != http.StatusNotFound {
		t.Errorf("single-tenant /signal/.../identity = %d, want 404 (reserved)", code)
	}
	if code := get("/dashboard"); code != http.StatusOK {
		t.Errorf("single-tenant ordinary SPA path = %d, want 200 (shell still served)", code)
	}
}

// Under multi-tenant the browser reaches the relay AS the home host, so the
// /wallet/* and /signal/{site}/identity routes must be re-registered on the
// HomeHost mux (Go gives a host pattern precedence over a host-less one). This
// asserts they resolve to the real handlers — not the home-host static catch-all.
func TestMultiTenantRoutesOnHomeHost(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:Bob", "host-bob", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	doHost := func(method, path string, body []byte) (int, []byte) {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(body))
		req.Host = "home.test" // reach the relay AS the home host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, b
	}

	// /signal/{site}/identity resolves on the home host → 200 (not the static 404/shell).
	if code, _ := doHost(http.MethodGet, "/signal/site:Bob/identity", nil); code != http.StatusOK {
		t.Fatalf("home-host /signal/site:Bob/identity = %d, want 200 (route shadowed by catch-all?)", code)
	}
	// PUT /wallet/{handle}/blob resolves on the home host → 200 (not 405 from the
	// GET-only static forwarder). Writer-authenticated: sign the canonical message.
	wpub, wpriv, _ := ed25519.GenerateKey(rand.Reader)
	ct, nonce := []byte("ciph"), []byte("nonc")
	sig := ed25519.Sign(wpriv, blobWriteMessage(testHandle, 1, nonce, ct))
	body, _ := json.Marshal(walletBlobIO{
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Version:    1,
		WritePub:   base64.StdEncoding.EncodeToString(wpub),
		Sig:        base64.StdEncoding.EncodeToString(sig),
	})
	if code, b := doHost(http.MethodPut, "/wallet/"+testHandle+"/blob", body); code != http.StatusOK {
		t.Fatalf("home-host PUT /wallet/.../blob = %d (%s), want 200", code, b)
	}
	// GET /wallet/{handle}/blob round-trips on the home host.
	if code, _ := doHost(http.MethodGet, "/wallet/"+testHandle+"/blob", nil); code != http.StatusOK {
		t.Fatalf("home-host GET /wallet/.../blob = %d, want 200", code)
	}
}

// Under -multi-tenant the anonymous home host serves ONLY the relay-disk
// bootstrap allowlist. Once the browser decrypts its directory it writes the
// opaque site_id routing cookie, and only then do static app GETs forward to the
// chosen Pi. /api/* remains refused in both states.
func TestRelayBootstrapManifestMatchesAllowlist(t *testing.T) {
	b, err := os.ReadFile("../../../web/relay-bootstrap-files.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		rel := strings.TrimSpace(line)
		if rel == "" || strings.HasPrefix(rel, "#") {
			continue
		}
		got, ok := homeBootstrapRelPath("/" + rel)
		if !ok {
			t.Fatalf("%s is in web/relay-bootstrap-files.txt but homeBootstrapRelPath rejects it", rel)
		}
		if got != rel {
			t.Fatalf("%s maps to %s, want exact manifest path", rel, got)
		}
	}
	for _, p := range []string{
		"/next-app.js",
		"/next.css",
		"/app.js",
		"/settings.js",
		"/components/index.js",
		"/components/ftw-price-chart.js",
		"/settings/tabs/access.js",
		"/plan.js",
		"/loadpoints.js",
	} {
		if rel, ok := homeBootstrapRelPath(p); ok {
			t.Fatalf("%s unexpectedly allowed in relay bootstrap as %s", p, rel)
		}
	}
}

func TestMultiTenantHomeBootstrapThenPiStaticForward(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "remote-loader.html"), []byte("<h1>LOADER</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p2p.js"), []byte("// bootstrap p2p"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Prove the allowlist, not the filesystem, controls app serving: even though
	// next-app.js exists on relay disk in this test, anonymous clients must not get
	// it from the relay.
	if err := os.WriteFile(filepath.Join(dir, "next-app.js"), []byte("RELAY APP"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "owner-access"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "owner-access", "enroll.html"), []byte("LOCAL ENROLL"), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := NewWalletBlobStore(t.TempDir(), 65536, 1024)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	relay := &Relay{
		Queue:            tunnel.NewQueue(),
		Tokens:           NewTokenRegistry(),
		Owners:           NewOwnerRegistry(),
		Polls:            NewPollSecrets(),
		Signals:          NewSignalMailbox(),
		Challenges:       NewSignalChallenges(),
		MultiTenant:      true,
		RequireDeviceKey: true,
		HomeHost:         "home.test",
		HomeWeb:          dir,
		WalletBlobs:      store,
		// NOTE: no HomeSite, no HomePubKey — multi-tenant has neither.
	}
	if err := relay.Owners.Register("site:Pi", "host-pi", "deadbeef"); err != nil {
		t.Fatalf("register owner: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Inner-Path", r.URL.Path)
		w.Header().Set("X-Saw-Cookie", r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "ftw_owner", Value: "must-not-leak", Path: "/"})
		_, _ = w.Write([]byte("PI:" + r.URL.Path))
	})
	host := tunnel.NewHost(srv.URL, "host-pi", backend)
	host.PollTimeout = time.Second
	host.SetPollSecret(mustIssue(t, relay.Polls, "host-pi"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go host.Run(ctx)

	routeCookie := &http.Cookie{Name: homeRouteSiteCookieName, Value: url.QueryEscape("site:Pi")}
	do := func(method, path string, cookies ...*http.Cookie) (int, string, http.Header) {
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		req.Host = "home.test"
		for _, c := range cookies {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b), resp.Header
	}

	// Anonymous "/" → the relay-disk loader, no Pi involved.
	if code, body, _ := do(http.MethodGet, "/"); code != 200 || body != "<h1>LOADER</h1>" {
		t.Fatalf(`GET "/" = %d %q, want 200 "<h1>LOADER</h1>" from bootstrap`, code, body)
	}
	// Anonymous app asset → not served from relay disk, even if the file exists.
	if code, body, _ := do(http.MethodGet, "/next-app.js"); code != http.StatusNotFound || strings.Contains(body, "RELAY APP") {
		t.Fatalf("anonymous /next-app.js = %d %q, want 404 and no relay app bytes", code, body)
	}
	// Routing cookie set by the loader → static app GETs forward to the selected Pi.
	if code, body, hdr := do(http.MethodGet, "/next-app.js", routeCookie); code != 200 || body != "PI:/next-app.js" {
		t.Fatalf("routed /next-app.js = %d %q, want Pi asset", code, body)
	} else {
		if hdr.Get("X-Saw-Cookie") != "" {
			t.Fatalf("relay forwarded browser cookies to Pi static request: %q", hdr.Get("X-Saw-Cookie"))
		}
		if got := hdr.Get("Cache-Control"); !strings.Contains(got, "private") || !strings.Contains(got, "max-age=300") {
			t.Fatalf("unversioned static Cache-Control = %q, want short private browser cache", got)
		}
		for _, sc := range hdr.Values("Set-Cookie") {
			if strings.HasPrefix(sc, "ftw_owner=") {
				t.Fatalf("relay leaked Pi owner cookie on static response: %q", sc)
			}
		}
	}
	if code, body, hdr := do(http.MethodGet, "/next-app.js?v=next18", routeCookie); code != 200 || body != "PI:/next-app.js" {
		t.Fatalf("routed versioned /next-app.js = %d %q, want Pi asset", code, body)
	} else if got := hdr.Get("Cache-Control"); !strings.Contains(got, "private") || !strings.Contains(got, "max-age=86400") {
		t.Fatalf("versioned static Cache-Control = %q, want long private browser cache", got)
	}
	if code, body, hdr := do(http.MethodGet, "/", routeCookie); code != 200 || body != "PI:/" {
		t.Fatalf("routed / = %d %q, want Pi shell", code, body)
	} else if got := hdr.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("routed shell Cache-Control = %q, want no-store", got)
	}
	// Owner-access bootstrap files stay local even with the route cookie.
	if code, body, _ := do(http.MethodGet, "/owner-access/enroll.html", routeCookie); code != 200 || body != "LOCAL ENROLL" {
		t.Fatalf("owner-access bootstrap = %d %q, want local relay bootstrap page", code, body)
	}
	// /api/* is still refused (owner data is P2P-only) — even /api/identity, which
	// under multi-tenant is per-site at /signal/{site}/identity, NOT here.
	if code, _, _ := do(http.MethodGet, "/api/owner-access/whoami", routeCookie); code != http.StatusForbidden {
		t.Fatalf("/api/* = %d, want 403 under multi-tenant", code)
	}
	if code, _, _ := do(http.MethodGet, "/api/identity", routeCookie); code != http.StatusForbidden {
		t.Fatalf("/api/identity over home host = %d, want 403 under multi-tenant (use /signal/{site}/identity)", code)
	}
	// A non-GET to the home host is refused (static assets are GET-only).
	if code, _, _ := do(http.MethodPost, "/anything", routeCookie); code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to home host = %d, want 405", code)
	}
}
