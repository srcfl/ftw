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

// claimKeyHex is hex(sha256(bootstrap_id)) — the 256-bit unguessable handle the
// browser derives from the URL fragment and presents to the relay. In tests we
// stand in for the high-entropy bootstrap_id with an arbitrary string; the relay
// never inspects the pre-image, only the 64-char hex digest.
func claimKeyHex(bootstrapID string) string {
	h := sha256.Sum256([]byte(bootstrapID))
	return hex.EncodeToString(h[:])
}

// signedBootstrapPut builds a Pi-signed PUT /bootstrap/{site_id} body and sends
// it. The sig is produced by the site identity over bootstrapPublishSigningString
// exactly as the Pi will (nova.Identity.SignRawHex → raw r||s hex), so the relay's
// verifyES256Hex check passes for the registered site key.
func signedBootstrapPut(t *testing.T, url, siteID string, descriptor []byte, claimKey string, tsMs int64, sig string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(bootstrapPublishIO{
		Descriptor: base64.StdEncoding.EncodeToString(descriptor),
		ClaimKey:   claimKey,
		TsMs:       tsMs,
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
// descriptor under site:A keyed by claim_key (= hex(sha256(bootstrap_id))) with a
// fresh ts_ms, then a fresh browser that knows the claim_key claims it back —
// descriptor and site_id round-trip verbatim.
func TestBootstrapPublishThenClaim(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:A", "host-a", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	descriptor := []byte(`{"v":1,"site":"site:A","pi_pubkey":"deadbeef"}`)
	claimKey := claimKeyHex("bootstrap-secret-A")
	tsMs := time.Now().UnixMilli()
	sig, err := id.SignRawHex(bootstrapPublishSigningString("site:A", claimKey, tsMs, descriptor))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	code, msg := signedBootstrapPut(t, srv.URL, "site:A", descriptor, claimKey, tsMs, sig)
	if code != http.StatusOK {
		t.Fatalf("publish: got %d (%s), want 200", code, msg)
	}

	// Claim with the matching claim_key.
	claimBody, _ := json.Marshal(map[string]string{"claim_key": claimKey})
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
// with the same claim_key finds nothing).
func TestBootstrapPublishBadSignature(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:A", "host-a", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	descriptor := []byte(`{"v":1,"site":"site:A"}`)
	claimKey := claimKeyHex("bootstrap-secret-bad")
	tsMs := time.Now().UnixMilli()
	// Garbage signature (well-formed hex length but not a real signature).
	garbage := strings.Repeat("ab", 64)
	code, _ := signedBootstrapPut(t, srv.URL, "site:A", descriptor, claimKey, tsMs, garbage)
	if code != http.StatusUnauthorized {
		t.Fatalf("bad-sig publish: got %d, want 401", code)
	}

	// Confirm nothing was parked.
	claimBody, _ := json.Marshal(map[string]string{"claim_key": claimKey})
	resp, err := http.Post(srv.URL+"/bootstrap/claim", "application/json", bytes.NewReader(claimBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("claim after bad-sig publish: got %d, want 404", resp.StatusCode)
	}
}

// TestBootstrapPublishStaleTimestamp: a PUT whose ts_ms is older than the 30 s
// replay window is rejected 400 even though its signature verifies — a captured
// publish body cannot be replayed once the window lapses. Nothing is parked.
func TestBootstrapPublishStaleTimestamp(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:A", "host-a", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	descriptor := []byte(`{"v":1,"site":"site:A"}`)
	claimKey := claimKeyHex("bootstrap-secret-stale")
	// 60 s in the past → outside the symmetric 30 s window. The signature is VALID
	// over this ts_ms, so a 400 proves the skew guard fired (not the sig check).
	tsMs := time.Now().Add(-60 * time.Second).UnixMilli()
	sig, err := id.SignRawHex(bootstrapPublishSigningString("site:A", claimKey, tsMs, descriptor))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	code, _ := signedBootstrapPut(t, srv.URL, "site:A", descriptor, claimKey, tsMs, sig)
	if code != http.StatusBadRequest {
		t.Fatalf("stale-ts publish: got %d, want 400", code)
	}

	// Confirm nothing was parked.
	claimBody, _ := json.Marshal(map[string]string{"claim_key": claimKey})
	resp, err := http.Post(srv.URL+"/bootstrap/claim", "application/json", bytes.NewReader(claimBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("claim after stale-ts publish: got %d, want 404", resp.StatusCode)
	}
}

// TestBootstrapPublishBadClaimKey: a PUT whose claim_key is missing or not a
// 64-char lowercase hex digest is rejected 400 (it must be hex(sha256(...))).
func TestBootstrapPublishBadClaimKey(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:A", "host-a", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	descriptor := []byte(`{"v":1}`)
	tsMs := time.Now().UnixMilli()
	for name, claimKey := range map[string]string{
		"empty":     "",
		"too-short": "abcd",
		"not-hex":   strings.Repeat("zz", 32),
		"uppercase": strings.ToUpper(strings.Repeat("ab", 32)),
	} {
		// The signature is irrelevant — the claim_key shape is rejected before any
		// crypto. Sign anyway so a 400 cannot be a sig artefact.
		sig, _ := id.SignRawHex(bootstrapPublishSigningString("site:A", claimKey, tsMs, descriptor))
		code, _ := signedBootstrapPut(t, srv.URL, "site:A", descriptor, claimKey, tsMs, sig)
		if code != http.StatusBadRequest {
			t.Fatalf("%s claim_key publish: got %d, want 400", name, code)
		}
	}
}

// TestBootstrapClaimNoMatch: a claim_key with no parked descriptor is a clean 404
// (the browser learns nothing about which sites exist).
func TestBootstrapClaimNoMatch(t *testing.T) {
	relay := newMultiTenantRelay(t)
	id := newTestIdentity(t)
	if err := relay.Owners.Register("site:A", "host-a", id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	descriptor := []byte(`{"v":1}`)
	claimKey := claimKeyHex("bootstrap-secret-match")
	tsMs := time.Now().UnixMilli()
	sig, _ := id.SignRawHex(bootstrapPublishSigningString("site:A", claimKey, tsMs, descriptor))
	if code, msg := signedBootstrapPut(t, srv.URL, "site:A", descriptor, claimKey, tsMs, sig); code != http.StatusOK {
		t.Fatalf("publish: got %d (%s), want 200", code, msg)
	}

	// Claim with a DIFFERENT claim_key → 404.
	claimBody, _ := json.Marshal(map[string]string{"claim_key": claimKeyHex("some-other-secret")})
	resp, err := http.Post(srv.URL+"/bootstrap/claim", "application/json", bytes.NewReader(claimBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("claim mismatched claim_key: got %d, want 404", resp.StatusCode)
	}
}

// TestBootstrapClaimBadClaimKey: a claim whose claim_key is missing or not a
// 64-char hex digest is rejected 400 before any store lookup.
func TestBootstrapClaimBadClaimKey(t *testing.T) {
	relay := newMultiTenantRelay(t)
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	for name, claimKey := range map[string]string{
		"empty":     "",
		"too-short": "abcd",
		"not-hex":   strings.Repeat("zz", 32),
	} {
		claimBody, _ := json.Marshal(map[string]string{"claim_key": claimKey})
		resp, err := http.Post(srv.URL+"/bootstrap/claim", "application/json", bytes.NewReader(claimBody))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s claim_key: got %d, want 400", name, resp.StatusCode)
		}
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
// path (already including any ?claim_key= / ?pin=). Returns the response (body
// drained).
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
// and parks a live bootstrap blob keyed by claim_key. It does NOT exercise the
// signed PUT path — that is covered above; here we just need a live blob so the
// enroll-forward gate opens.
func publishLiveBootstrap(t *testing.T, relay *Relay, site, hostID, claimKey string) {
	t.Helper()
	id := newTestIdentity(t)
	if err := relay.Owners.Register(site, hostID, id.PublicKeyHex()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := relay.Bootstrap.Put(site, []byte(`{"site":"`+site+`"}`), claimKey, time.Minute); err != nil {
		t.Fatalf("bootstrap put: %v", err)
	}
}

// TestBootstrapEnrollForwardHappyPath (a): a browser that holds the live claim_key
// can POST enroll/start through the relay; the relay gates on claim_key and
// forwards ?pin onward to the Pi (which validates it). Returns 200.
func TestBootstrapEnrollForwardHappyPath(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	claimKey := claimKeyHex("bootstrap-secret-happy")
	srv, seen := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	resp, body := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?claim_key="+claimKey+"&pin="+pin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll/start: got %d (%s), want 200", resp.StatusCode, body)
	}
	// The Pi must have seen the enroll/start inner path with ONLY the pin forwarded
	// (the relay-only claim_key is stripped at the boundary).
	got := seen()
	if len(got) != 1 || got[0] != "/api/owner-access/enroll/start?pin="+pin {
		t.Fatalf("Pi saw %v, want exactly [/api/owner-access/enroll/start?pin=%s]", got, pin)
	}
}

// TestBootstrapEnrollForwardNoLiveClaimKey (b): a claim_key with no live bootstrap
// is a 403 and nothing is forwarded to the Pi.
func TestBootstrapEnrollForwardNoLiveClaimKey(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	claimKey := claimKeyHex("bootstrap-secret-live")
	srv, seen := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	wrongKey := claimKeyHex("not-the-live-secret")
	resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?claim_key="+wrongKey+"&pin="+pin)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("enroll/start wrong claim_key: got %d, want 403", resp.StatusCode)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("Pi was forwarded %v on a dead claim_key; want nothing", got)
	}
}

// TestBootstrapEnrollForwardBurnAfterFinish (c): a successful enroll/finish burns
// the bootstrap (single-use). After that the blob is no longer Live and a second
// enroll/start with the same claim_key is rejected 403.
func TestBootstrapEnrollForwardBurnAfterFinish(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	claimKey := claimKeyHex("bootstrap-secret-burn")
	srv, _ := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	// finish returns 200 → the relay burns the blob.
	resp, body := enrollPost(t, srv.URL, "/api/owner-access/enroll/finish?claim_key="+claimKey+"&pin="+pin)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll/finish: got %d (%s), want 200", resp.StatusCode, body)
	}
	if relay.Bootstrap.Live(site, claimKey) {
		t.Fatal("bootstrap still Live after a 200 enroll/finish; must be burned")
	}
	// A replay of the flow with the same claim_key now fails at the gate.
	resp2, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?claim_key="+claimKey+"&pin="+pin)
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
	claimKey := claimKeyHex("bootstrap-secret-scope")
	srv, seen := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	// /api/owner-access/whoami is owner data — must never traverse the relay. It
	// is not an enroll path, so it falls through to homeStaticForward, which 403s
	// every /api/* under multi-tenant (GET path). Use GET so we land on that gate
	// rather than the non-GET 405 backstop.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/owner-access/whoami?claim_key="+claimKey+"&pin="+pin, nil)
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
	claimKey := claimKeyHex("bootstrap-secret-cookie")
	srv, _ := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	resp, body := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?claim_key="+claimKey+"&pin="+pin)
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

	resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?claim_key="+claimKeyHex("x")+"&pin=123456")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no bootstrap store: got %d, want 503", resp.StatusCode)
	}
}

// TestBootstrapEnrollForwardNoClaimKey: 403 when the claim_key query param is
// absent (the relay gates on claim_key, not pin).
func TestBootstrapEnrollForwardNoClaimKey(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	claimKey := claimKeyHex("bootstrap-secret-nokey")
	srv, _ := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?pin="+pin)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing claim_key: got %d, want 403", resp.StatusCode)
	}
}

// TestBootstrapEnrollForwardHomeOffline: 503 when the resolved site's Pi is not
// registered / not fresh (so the relay can't reach a home to enroll against).
func TestBootstrapEnrollForwardHomeOffline(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, pin = "site:A", "123456"
	claimKey := claimKeyHex("bootstrap-secret-offline")
	srv := httptest.NewServer(relay.Handler())
	t.Cleanup(srv.Close)
	// Park a live bootstrap but DO NOT register the site's host → Owners.Active
	// reports !registered.
	if err := relay.Bootstrap.Put(site, []byte(`{"site":"`+site+`"}`), claimKey, time.Minute); err != nil {
		t.Fatalf("bootstrap put: %v", err)
	}

	resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?claim_key="+claimKey+"&pin="+pin)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("home offline: got %d, want 503", resp.StatusCode)
	}
}
