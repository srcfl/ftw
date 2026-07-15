package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/tunnel"
)

// newMultiTenantRelay builds a relay in multi-tenant mode with a real
// WalletBlobStore backed by a temp dir, ready for HTTP-level tests.
func newMultiTenantRelay(t *testing.T) *Relay {
	t.Helper()
	store, err := NewWalletBlobStore(t.TempDir(), 65536, 1024)
	if err != nil {
		t.Fatalf("NewWalletBlobStore: %v", err)
	}
	return &Relay{
		Queue:            tunnel.NewQueue(),
		Tokens:           NewTokenRegistry(),
		Owners:           NewOwnerRegistry(),
		Polls:            NewPollSecrets(),
		Signals:          NewSignalMailbox(),
		Challenges:       NewSignalChallenges(),
		MultiTenant:      true,
		RequireDeviceKey: true,
		HomeHost:         "home.test",
		HomeWeb:          t.TempDir(),
		WalletBlobs:      store,
		Bootstrap:        NewBootstrapStore(65536, 4096),
	}
}

// b64 standard-base64-encodes bytes for the opaque wire fields.
func b64b(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
func b64(s string) string  { return b64b([]byte(s)) }

// signedBlobPut builds a writer-authenticated PUT body and sends it. The body's
// write_pub/sig are produced from (pub, priv) over the canonical message, so the
// relay's Ed25519 check passes when the key is the wallet's pinned one.
func signedBlobPut(t *testing.T, url, handle string, ct, nonce []byte, version int, pub ed25519.PublicKey, priv ed25519.PrivateKey) (int, string) {
	t.Helper()
	sig := ed25519.Sign(priv, blobWriteMessage(handle, version, nonce, ct))
	body, _ := json.Marshal(walletBlobIO{
		Ciphertext: b64b(ct), Nonce: b64b(nonce), Version: version,
		WritePub: b64b(pub), Sig: b64b(sig),
	})
	return rawBlobPut(t, url, handle, body)
}

func rawBlobPut(t *testing.T, url, handle string, body []byte) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url+"/wallet/"+handle+"/blob", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// PUT then GET round-trips the opaque ciphertext blob; version monotonicity is
// enforced (a stale PUT is 409); a bad handle is 400; an unknown handle GET is 404.
func TestWalletBlobPutGetHTTP(t *testing.T) {
	srv := httptest.NewServer(newMultiTenantRelay(t).Handler())
	defer srv.Close()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	get := func(handle string) (int, walletBlobIO) {
		resp, err := http.Get(srv.URL + "/wallet/" + handle + "/blob")
		if err != nil {
			t.Fatal(err)
		}
		var out walletBlobIO
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		return resp.StatusCode, out
	}

	// Unknown handle → 404.
	if code, _ := get(testHandle); code != http.StatusNotFound {
		t.Fatalf("GET unknown handle = %d, want 404", code)
	}
	// First PUT (version 1) → 200.
	if code, body := signedBlobPut(t, srv.URL, testHandle, []byte("cipher-1"), []byte("nonce-1"), 1, pub, priv); code != http.StatusOK {
		t.Fatalf("PUT v1 = %d (%s), want 200", code, body)
	}
	// GET returns it verbatim.
	code, out := get(testHandle)
	if code != http.StatusOK || out.Ciphertext != b64("cipher-1") || out.Nonce != b64("nonce-1") || out.Version != 1 {
		t.Fatalf("GET v1 = %d %+v, want 200 cipher-1/nonce-1/v1", code, out)
	}
	// PUT v2 advances (same pinned key).
	if code, body := signedBlobPut(t, srv.URL, testHandle, []byte("cipher-2"), []byte("nonce-2"), 2, pub, priv); code != http.StatusOK {
		t.Fatalf("PUT v2 = %d (%s), want 200", code, body)
	}
	// A stale PUT (valid sig, version <= stored) is rejected 409.
	if code, _ := signedBlobPut(t, srv.URL, testHandle, []byte("stale"), []byte("stale"), 2, pub, priv); code != http.StatusConflict {
		t.Fatalf("PUT stale v2 = %d, want 409", code)
	}
	if code, _ := signedBlobPut(t, srv.URL, testHandle, []byte("stale"), []byte("stale"), 1, pub, priv); code != http.StatusConflict {
		t.Fatalf("PUT rollback v1 = %d, want 409", code)
	}
	// A bad handle is 400 on both verbs (checked before the body).
	if code, _ := get("short"); code != http.StatusBadRequest {
		t.Fatalf("GET bad handle = %d, want 400", code)
	}
	if code, _ := signedBlobPut(t, srv.URL, "short", []byte("x"), []byte("y"), 1, pub, priv); code != http.StatusBadRequest {
		t.Fatalf("PUT bad handle = %d, want 400", code)
	}
	// Non-base64 ciphertext is a 400 (transport-encoding validation).
	badBody, _ := json.Marshal(map[string]any{"ciphertext": "!!!not-base64!!!", "nonce": b64("y"), "version": 9, "write_pub": b64b(pub), "sig": b64("z")})
	if code, _ := rawBlobPut(t, srv.URL, testHandle, badBody); code != http.StatusBadRequest {
		t.Fatalf("PUT non-base64 ciphertext = %d, want 400", code)
	}
}

// Writer authentication over HTTP: a PUT whose signature does not verify against
// the wallet's pinned write key is 403 — a userHandle-knower cannot overwrite.
func TestWalletBlobWriterAuthHTTP(t *testing.T) {
	srv := httptest.NewServer(newMultiTenantRelay(t).Handler())
	defer srv.Close()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	// Owner's first write pins the key.
	if code, body := signedBlobPut(t, srv.URL, testHandle, []byte("ct"), []byte("nc"), 1, pub, priv); code != http.StatusOK {
		t.Fatalf("owner PUT = %d (%s), want 200", code, body)
	}
	// An attacker who learned the handle but has a DIFFERENT key is 403.
	apub, apriv, _ := ed25519.GenerateKey(rand.Reader)
	if code, _ := signedBlobPut(t, srv.URL, testHandle, []byte("evil"), []byte("nc"), 2, apub, apriv); code != http.StatusForbidden {
		t.Fatalf("attacker takeover = %d, want 403", code)
	}
	// A garbage signature with the owner's pubkey is also 403.
	body, _ := json.Marshal(walletBlobIO{
		Ciphertext: b64("x"), Nonce: b64("nc"), Version: 2,
		WritePub: b64b(pub), Sig: b64b(make([]byte, 64)),
	})
	if code, _ := rawBlobPut(t, srv.URL, testHandle, body); code != http.StatusForbidden {
		t.Fatalf("garbage sig = %d, want 403", code)
	}
}

// With multi-tenant OFF the wallet-blob + per-site identity routes are NOT
// registered at all — a request gets a plain 404 (no 503, no public-key answer),
// so the dormant feature adds no surface.
func TestWalletRoutesAbsentWhenFlagOff(t *testing.T) {
	srv := httptest.NewServer(newTestRelay().Handler())
	defer srv.Close()
	for _, path := range []string{
		"/wallet/" + testHandle + "/blob",
		"/signal/site:whatever/identity",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with multi-tenant off = %d, want 404 (route must not be registered)", path, resp.StatusCode)
		}
	}
}

// The blob endpoints are CORS-enabled so the LAN enrollment page (served from the
// Pi's own origin) can write the first directory blob to the relay cross-origin.
func TestWalletBlobCORS(t *testing.T) {
	srv := httptest.NewServer(newMultiTenantRelay(t).Handler())
	defer srv.Close()
	// Preflight → 204 with permissive ACAO + the methods/headers it'll use.
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/wallet/"+testHandle+"/blob", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS preflight = %d, want 204", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight ACAO = %q, want *", resp.Header.Get("Access-Control-Allow-Origin"))
	}
	// A GET also carries the header (so the cross-origin fetch can read the body).
	r2, _ := http.Get(srv.URL + "/wallet/" + testHandle + "/blob")
	r2.Body.Close()
	if r2.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("GET ACAO = %q, want *", r2.Header.Get("Access-Control-Allow-Origin"))
	}
}

// An over-max ciphertext is rejected with 413, never buffered into the store
// (the size cap is enforced before the write-auth check).
func TestWalletBlobPutTooLargeHTTP(t *testing.T) {
	store, err := NewWalletBlobStore(t.TempDir(), 128, 1024) // tiny 128-byte cap
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	relay := newMultiTenantRelay(t)
	relay.WalletBlobs = store
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	big := b64(strings.Repeat("x", 4096)) // base64 of 4 KiB, well over the 128-byte cap
	body, _ := json.Marshal(walletBlobIO{Ciphertext: big, Nonce: b64("n"), Version: 1, WritePub: b64("p"), Sig: b64("s")})
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/wallet/"+testHandle+"/blob", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-max PUT = %d, want 413", resp.StatusCode)
	}
}

// GET /signal/{site_id}/identity returns the PUBLIC key the relay holds for that
// site. A known site → 200; an unknown site → 404; never a secret.
func TestSignalIdentityHTTP(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:Alice", "host-alice", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/signal/site:Alice/identity")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("identity for known site = %d, want 200", resp.StatusCode)
	}
	var out struct {
		PublicKeyHex string `json:"public_key_hex"`
		SiteID       string `json:"site_id"`
		Algorithm    string `json:"algorithm"`
		Curve        string `json:"curve"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if out.PublicKeyHex != id.PublicKeyHex() {
		t.Fatalf("public_key_hex = %q, want the registered key", out.PublicKeyHex)
	}
	if out.SiteID != "site:Alice" || out.Algorithm != "ES256" || out.Curve != "P-256" {
		t.Fatalf("identity fields = %+v, want site:Alice/ES256/P-256", out)
	}

	resp2, err := http.Get(srv.URL + "/signal/site:nobody/identity")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("identity for unknown site = %d, want 404", resp2.StatusCode)
	}
}
