package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
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

// ---- Hardened bootstrap enroll-forward (Task 4) ----

// enrollStubPi spins a fake Pi behind the tunnel for the given host_id. The
// backend records every inner path it is asked to serve (so a test can assert
// the relay only ever forwards enroll/* to it) and ALWAYS sets a Set-Cookie the
// relay must strip before the response reaches the browser. It returns the
// started httptest server and a function to read the recorded inner paths.
func enrollStubPi(t *testing.T, relay *Relay, hostID string) (*httptest.Server, func() []string) {
	t.Helper()
	srv := httptest.NewServer(relay.Handler())
	t.Cleanup(srv.Close)

	var mu sync.Mutex
	var seen []string
	backend := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		if req.URL.RawQuery != "" {
			p += "?" + req.URL.RawQuery
		}
		mu.Lock()
		seen = append(seen, p)
		mu.Unlock()
		// The Pi sets the owner session cookie on a successful enroll; the relay
		// MUST strip it so it never traverses the relay-visible response.
		http.SetCookie(w, &http.Cookie{Name: "ftw_owner", Value: "session-secret", Path: "/"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	host := tunnel.NewHost(srv.URL, hostID, backend)
	host.PollTimeout = time.Second
	host.SetPollSecret(mustIssue(t, relay.Polls, hostID))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go host.Run(ctx)

	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

// enrollPost issues a POST to the home host as a browser would, with the given
// path (already including any ?pin=). Returns the response (body drained).
func enrollPost(t *testing.T, srvURL, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "home.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// publishLiveBootstrap registers the site+host (so Owners.Active resolves fresh)
// and parks a live bootstrap blob keyed by sha256(pin). It does NOT exercise the
// signed PUT path — that is covered above; here we just need a live blob so the
// enroll-forward gate opens.
func publishLiveBootstrap(t *testing.T, relay *Relay, site, hostID, pin string) {
	t.Helper()
	id := newTestIdentity(t)
	if err := relay.Owners.Register(site, hostID, id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := relay.Bootstrap.Put(site, []byte(`{"site":"`+site+`"}`), pinHashHex(pin), time.Minute); err != nil {
		t.Fatalf("bootstrap put: %v", err)
	}
}

// TestBootstrapEnrollForwardHappyPath (a): a browser that knows the live PIN can
// POST enroll/start through the relay; it is forwarded to the Pi and returns 200.
func TestBootstrapEnrollForwardHappyPath(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	srv, seen := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, pin)

	resp, body := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?pin="+pin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll/start: got %d (%s), want 200", resp.StatusCode, body)
	}
	// The Pi must have seen the enroll/start inner path with the pin forwarded.
	got := seen()
	if len(got) != 1 || got[0] != "/api/owner-access/enroll/start?pin="+pin {
		t.Fatalf("Pi saw %v, want exactly [/api/owner-access/enroll/start?pin=%s]", got, pin)
	}
}

// TestBootstrapEnrollForwardNoLivePin (b): a PIN with no live bootstrap is a 403
// and nothing is forwarded to the Pi.
func TestBootstrapEnrollForwardNoLivePin(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	srv, seen := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, pin)

	resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?pin=000000")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("enroll/start wrong pin: got %d, want 403", resp.StatusCode)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("Pi was forwarded %v on a dead pin; want nothing", got)
	}
}

// TestBootstrapEnrollForwardBurnAfterFinish (c): a successful enroll/finish burns
// the bootstrap (single-use). After that the blob is no longer Live and a second
// enroll/start with the same pin is rejected 403.
func TestBootstrapEnrollForwardBurnAfterFinish(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	srv, _ := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, pin)

	// finish returns 200 → the relay burns the blob.
	resp, body := enrollPost(t, srv.URL, "/api/owner-access/enroll/finish?pin="+pin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll/finish: got %d (%s), want 200", resp.StatusCode, body)
	}
	if relay.Bootstrap.Live(site, pinHashHex(pin)) {
		t.Fatal("bootstrap still Live after a 200 enroll/finish; must be burned")
	}
	// A replay of the flow with the same pin now fails at the gate.
	resp2, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?pin="+pin)
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("enroll/start after burn: got %d, want 403", resp2.StatusCode)
	}
}

// TestBootstrapEnrollForwardScopedToEnroll (d): a non-enroll home-host /api path
// is NOT handled by the enroll-forward — it falls through to homeStaticForward,
// which 403s all /api/* under multi-tenant (proving the forward is enroll-only).
func TestBootstrapEnrollForwardScopedToEnroll(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	srv, seen := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, pin)

	// /api/owner-access/whoami is owner data — must never traverse the relay. It
	// is not an enroll path, so it falls through to homeStaticForward, which 403s
	// every /api/* under multi-tenant (GET path). Use GET so we land on that gate
	// rather than the non-GET 405 backstop.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/owner-access/whoami?pin="+pin, nil)
	req.Host = "home.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("whoami via relay: got %d, want 403 (P2P-only)", resp.StatusCode)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("non-enroll path was forwarded to the Pi: %v", got)
	}
}

// TestBootstrapEnrollForwardStripsSetCookie (e): even though the stub Pi sets an
// ftw_owner cookie on the enroll response, the relay strips it so the owner
// session secret never appears on a relay-visible response.
func TestBootstrapEnrollForwardStripsSetCookie(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	srv, _ := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, pin)

	resp, body := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?pin="+pin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll/start: got %d (%s), want 200", resp.StatusCode, body)
	}
	if sc := resp.Header.Values("Set-Cookie"); len(sc) != 0 {
		t.Fatalf("Set-Cookie leaked through the relay: %v", sc)
	}
}

// TestBootstrapEnrollForwardNoStore: 503 when the bootstrap store isn't wired.
func TestBootstrapEnrollForwardNoStore(t *testing.T) {
	relay := newMultiTenantRelay(t)
	relay.Bootstrap = nil
	srv := httptest.NewServer(relay.Handler())
	t.Cleanup(srv.Close)

	resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?pin=123456")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no bootstrap store: got %d, want 503", resp.StatusCode)
	}
}

// TestBootstrapEnrollForwardNoPin: 403 when the pin query param is absent.
func TestBootstrapEnrollForwardNoPin(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	srv, _ := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, pin)

	resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing pin: got %d, want 403", resp.StatusCode)
	}
}

// TestBootstrapEnrollForwardHomeOffline: 503 when the resolved site's Pi is not
// registered / not fresh (so the relay can't reach a home to enroll against).
func TestBootstrapEnrollForwardHomeOffline(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, pin = "site:A", "123456"
	srv := httptest.NewServer(relay.Handler())
	t.Cleanup(srv.Close)
	// Park a live bootstrap but DO NOT register the site's host → Owners.Active
	// reports !registered.
	if err := relay.Bootstrap.Put(site, []byte(`{"site":"`+site+`"}`), pinHashHex(pin), time.Minute); err != nil {
		t.Fatalf("bootstrap put: %v", err)
	}

	resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?pin="+pin)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("home offline: got %d, want 503", resp.StatusCode)
	}
}
