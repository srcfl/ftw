package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// pinHashHex is the claim key the Pi commits to and the browser presents: the
// hex-encoded SHA-256 of the human PIN. The relay never sees the PIN itself.
func pinHashHex(pin string) string {
	h := sha256.Sum256([]byte(pin))
	return hex.EncodeToString(h[:])
}

// signedBootstrapPut builds a Pi-signed PUT /bootstrap/{site_id} body and sends
// it. The sig is produced by the site identity over bootstrapPublishSigningString
// exactly as the Pi will (nova.Identity.SignRawHex → raw r||s hex), so the relay's
// verifyES256Hex check passes for the registered site key.
func signedBootstrapPut(t *testing.T, url, siteID string, descriptor []byte, pinHash, sig string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(bootstrapPublishIO{
		Descriptor: base64.StdEncoding.EncodeToString(descriptor),
		PinHash:    pinHash,
		Sig:        sig,
	})
	req, _ := http.NewRequest(http.MethodPut, url+"/bootstrap/"+siteID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// TestBootstrapPublishThenClaim is the happy path: the Pi publishes its signed
// descriptor under site:A keyed by sha256(PIN), then a fresh browser that knows the
// PIN claims it back — descriptor and site_id round-trip verbatim.
func TestBootstrapPublishThenClaim(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:A", "host-a", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	descriptor := []byte(`{"v":1,"site":"site:A","pi_pubkey":"deadbeef"}`)
	pin := "428317"
	pinHash := pinHashHex(pin)
	sig, err := id.SignRawHex(bootstrapPublishSigningString("site:A", pinHash, descriptor))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	code, msg := signedBootstrapPut(t, srv.URL, "site:A", descriptor, pinHash, sig)
	if code != http.StatusOK {
		t.Fatalf("publish: got %d (%s), want 200", code, msg)
	}

	// Claim with the matching PIN.
	claimBody, _ := json.Marshal(map[string]string{"pin": pin})
	resp, err := http.Post(srv.URL+"/bootstrap/claim", "application/json", bytes.NewReader(claimBody))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: got %d (%s), want 200", resp.StatusCode, string(b))
	}
	var out struct {
		SiteID     string `json:"site_id"`
		Descriptor string `json:"descriptor"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode claim: %v (%s)", err, string(b))
	}
	if out.SiteID != "site:A" {
		t.Fatalf("claim site_id = %q, want site:A", out.SiteID)
	}
	if out.Descriptor != string(descriptor) {
		t.Fatalf("claim descriptor = %q, want %q", out.Descriptor, string(descriptor))
	}
}

// TestBootstrapPublishBadSignature: a PUT whose signature does not verify against
// the registered site key is rejected 401 and nothing is parked (a later claim
// with the same PIN finds nothing).
func TestBootstrapPublishBadSignature(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:A", "host-a", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	descriptor := []byte(`{"v":1,"site":"site:A"}`)
	pinHash := pinHashHex("999999")
	// Garbage signature (well-formed hex length but not a real signature).
	garbage := strings.Repeat("ab", 64)
	code, _ := signedBootstrapPut(t, srv.URL, "site:A", descriptor, pinHash, garbage)
	if code != http.StatusUnauthorized {
		t.Fatalf("bad-sig publish: got %d, want 401", code)
	}

	// Confirm nothing was parked.
	claimBody, _ := json.Marshal(map[string]string{"pin": "999999"})
	resp, err := http.Post(srv.URL+"/bootstrap/claim", "application/json", bytes.NewReader(claimBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("claim after bad-sig publish: got %d, want 404", resp.StatusCode)
	}
}

// TestBootstrapClaimNoMatch: a claim PIN that hashes to no parked descriptor is a
// clean 404 (the browser learns nothing).
func TestBootstrapClaimNoMatch(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:A", "host-a", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	descriptor := []byte(`{"v":1}`)
	pinHash := pinHashHex("111111")
	sig, _ := id.SignRawHex(bootstrapPublishSigningString("site:A", pinHash, descriptor))
	if code, msg := signedBootstrapPut(t, srv.URL, "site:A", descriptor, pinHash, sig); code != http.StatusOK {
		t.Fatalf("publish: got %d (%s), want 200", code, msg)
	}

	// Claim with a DIFFERENT pin → 404.
	claimBody, _ := json.Marshal(map[string]string{"pin": "222222"})
	resp, err := http.Post(srv.URL+"/bootstrap/claim", "application/json", bytes.NewReader(claimBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("claim mismatched pin: got %d, want 404", resp.StatusCode)
	}
}
