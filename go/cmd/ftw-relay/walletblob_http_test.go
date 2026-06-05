package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
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
	}
}

// b64 standard-base64-encodes a string so a test can build the opaque
// ciphertext/nonce wire fields the relay round-trips without parsing.
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// PUT then GET round-trips the opaque ciphertext blob; version monotonicity is
// enforced (a stale PUT is 409); a bad handle is 400; an unknown handle GET is 404.
func TestWalletBlobPutGetHTTP(t *testing.T) {
	srv := httptest.NewServer(newMultiTenantRelay(t).Handler())
	defer srv.Close()

	put := func(handle string, ct, nonce string, version int) (int, string) {
		body, _ := json.Marshal(walletBlobIO{Ciphertext: ct, Nonce: nonce, Version: version})
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/wallet/"+handle+"/blob", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
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
	if code, body := put(testHandle, b64("cipher-1"), b64("nonce-1"), 1); code != http.StatusOK {
		t.Fatalf("PUT v1 = %d (%s), want 200", code, body)
	}
	// GET returns it verbatim.
	code, out := get(testHandle)
	if code != http.StatusOK || out.Ciphertext != b64("cipher-1") || out.Nonce != b64("nonce-1") || out.Version != 1 {
		t.Fatalf("GET v1 = %d %+v, want 200 cipher-1/nonce-1/v1", code, out)
	}
	// PUT v2 advances.
	if code, body := put(testHandle, b64("cipher-2"), b64("nonce-2"), 2); code != http.StatusOK {
		t.Fatalf("PUT v2 = %d (%s), want 200", code, body)
	}
	// A stale PUT (version <= stored) is rejected 409.
	if code, _ := put(testHandle, b64("stale"), b64("stale"), 2); code != http.StatusConflict {
		t.Fatalf("PUT stale v2 = %d, want 409", code)
	}
	if code, _ := put(testHandle, b64("stale"), b64("stale"), 1); code != http.StatusConflict {
		t.Fatalf("PUT rollback v1 = %d, want 409", code)
	}
	// A bad handle is 400 on both verbs.
	if code, _ := get("short"); code != http.StatusBadRequest {
		t.Fatalf("GET bad handle = %d, want 400", code)
	}
	if code, _ := put("short", b64("x"), b64("y"), 1); code != http.StatusBadRequest {
		t.Fatalf("PUT bad handle = %d, want 400", code)
	}
	// Non-base64 ciphertext is a 400 (the relay never parses plaintext, but it
	// validates the transport encoding so a garbage body is caught here).
	if code, _ := put(testHandle, "!!!not-base64!!!", b64("y"), 9); code != http.StatusBadRequest {
		t.Fatalf("PUT non-base64 ciphertext = %d, want 400", code)
	}
}

// An over-max ciphertext is rejected with 413, never buffered into the store.
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
	body, _ := json.Marshal(walletBlobIO{Ciphertext: big, Nonce: b64("n"), Version: 1})
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
// site (a convenience cross-check; the browser already pins from the Pi-signed
// directory entry). A known site → 200 with the registered key; an unknown site
// → 404; never a secret.
func TestSignalIdentityHTTP(t *testing.T) {
	relay := newMultiTenantRelay(t)
	// TOFU-register a site so the relay holds its public key.
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

	// Unknown site → 404.
	resp2, err := http.Get(srv.URL + "/signal/site:nobody/identity")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("identity for unknown site = %d, want 404", resp2.StatusCode)
	}
}
