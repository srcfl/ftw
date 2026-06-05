package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// home_web_test.go — SLICE 1 (serve the home shell from the relay's -home-web
// dir), SLICE 2 (answer /api/identity from the pinned home pubkey), and C1
// (/me/register publishes the site's trusted device keys).

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
	sig, err := id.SignRawHex(tunnel.MeRegisterSigningString("site:Home", "host-owner", tsMs))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body, _ := json.Marshal(meRegisterRequest{
		SiteID: "site:Home", HostID: "host-owner",
		PublicKey: id.PublicKeyHex(), TsMs: tsMs, Sig: sig,
		DevicePubkeys: []string{
			d1.pubKeyHex,
			d2.pubKeyHex,
			d1.pubKeyHex, // duplicate — must be de-duped, not double-stored
			"not-a-key",  // malformed — must be dropped, not reject the whole reg
		},
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
		sig, _ := id.SignRawHex(tunnel.MeRegisterSigningString("site:Home", "host-owner", tsMs))
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
