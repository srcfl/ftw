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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/tunnel"
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

// TestBootstrapEnrollForwardPreservesFinishQuery (a'): the browser sends
// ceremony_token + name (and pin) ONLY in the query string of the enroll/finish
// POST — never in the body. The relay must forward the FULL query (minus the
// relay-private claim_key) so the Pi's handleOwnerEnrollFinish can read
// ceremony_token; a regression to a hardcoded "?pin=" silently drops it and the
// Pi 400s "ceremony_token required", so multi-tenant enroll can never complete.
// We drive a real enroll/finish with all four params and assert the Pi saw every
// browser param (url-decoded) and did NOT see claim_key. The values include a
// space + reserved char in `name` to prove q.Encode() escapes them cleanly across
// the tunnel host's URL re-parse (the latent query-injection footgun).
func TestBootstrapEnrollForwardPreservesFinishQuery(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	const ceremony = "ceremony-tok-abc123"
	const name = "Erik's iPhone & iPad" // space + '&' must NOT inject extra params
	claimKey := claimKeyHex("bootstrap-secret-finishq")
	srv, seen := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	q := url.Values{}
	q.Set("ceremony_token", ceremony)
	q.Set("name", name)
	q.Set("claim_key", claimKey)
	q.Set("pin", pin)
	resp, body := enrollPost(t, srv.URL, "/api/owner-access/enroll/finish?"+q.Encode())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll/finish: got %d (%s), want 200", resp.StatusCode, body)
	}
	got := seen()
	if len(got) != 1 {
		t.Fatalf("Pi saw %v, want exactly one forwarded path", got)
	}
	// Parse the forwarded inner path back to its query so we assert on decoded
	// values (not on a byte-exact encoding, which url.Values.Encode key-sorts).
	u, err := url.Parse(got[0])
	if err != nil {
		t.Fatalf("forwarded inner path not parseable: %q (%v)", got[0], err)
	}
	if u.Path != "/api/owner-access/enroll/finish" {
		t.Fatalf("forwarded path = %q, want /api/owner-access/enroll/finish", u.Path)
	}
	fq := u.Query()
	if fq.Get("ceremony_token") != ceremony {
		t.Fatalf("forwarded ceremony_token = %q, want %q (relay dropped it — Pi would 400)", fq.Get("ceremony_token"), ceremony)
	}
	if fq.Get("name") != name {
		t.Fatalf("forwarded name = %q, want %q (escaping mangled it across the tunnel re-parse)", fq.Get("name"), name)
	}
	if fq.Get("pin") != pin {
		t.Fatalf("forwarded pin = %q, want %q", fq.Get("pin"), pin)
	}
	if fq.Has("claim_key") {
		t.Fatalf("relay-private claim_key leaked to the Pi in %q", got[0])
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
	// A second enroll/finish with the same claim_key also 403s at the gate — the
	// window closed on the first accepted finish (Reserve-then-Burn).
	resp3, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/finish?claim_key="+claimKey+"&pin="+pin)
	if resp3.StatusCode != http.StatusForbidden {
		t.Fatalf("second enroll/finish after consume: got %d, want 403", resp3.StatusCode)
	}
}

// configurableStubPi spins a fake Pi behind the tunnel whose response status is
// controlled by `status` and whose handler optionally blocks on `gate` (closed by
// the test) before answering. This lets a test stage two concurrent finishes that
// truly contend at the relay's Reserve gate, or stage a Pi non-200 so the relay
// must Release. It records every inner path it served.
func configurableStubPi(t *testing.T, relay *Relay, hostID string, status *atomic.Int32, gate <-chan struct{}) (*httptest.Server, func() []string) {
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
		if gate != nil {
			<-gate
		}
		w.WriteHeader(int(status.Load()))
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

// TestBootstrapEnrollForwardConcurrentDoubleFinish (b): two concurrent in-flight
// enroll/finish forwards through the relay must not BOTH enroll — the relay
// RESERVES the bootstrap BEFORE forwarding, so exactly one finish forwards to the
// Pi (200) and the other is refused 403 at the reserve gate, never reaching the
// Pi. We hold the Pi's response open with a gate so the second finish is forced to
// contend with the first while the first is still in flight.
func TestBootstrapEnrollForwardConcurrentDoubleFinish(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	claimKey := claimKeyHex("bootstrap-secret-concurrent")
	var status atomic.Int32
	status.Store(http.StatusOK)
	gate := make(chan struct{})
	srv, seen := configurableStubPi(t, relay, hostID, &status, gate)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	const racers = 8
	var wg sync.WaitGroup
	codes := make([]int, racers)
	begin := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-begin
			resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/finish?claim_key="+claimKey+"&pin="+pin)
			codes[i] = resp.StatusCode
		}(i)
	}
	close(begin)
	// Give the racers a beat to all hit the relay's Reserve gate; exactly one
	// reserves and forwards to the (still-gated) Pi, the rest 403 immediately.
	time.Sleep(100 * time.Millisecond)
	close(gate) // let the single forwarded finish complete
	wg.Wait()

	var ok, forbidden int
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusForbidden:
			forbidden++
		default:
			t.Fatalf("unexpected finish status %d (codes=%v)", c, codes)
		}
	}
	if ok != 1 || forbidden != racers-1 {
		t.Fatalf("concurrent finishes: %d×200 / %d×403, want exactly 1×200 / %d×403 (codes=%v)", ok, forbidden, racers-1, codes)
	}
	// The Pi must have been forwarded the finish exactly once — the reserve gate
	// kept the losers off the Pi entirely.
	if got := seen(); len(got) != 1 {
		t.Fatalf("Pi was forwarded %d finishes, want exactly 1: %v", len(got), got)
	}
	// And the window is burned (the single accepted finish closed it).
	if relay.Bootstrap.Live(site, claimKey) {
		t.Fatal("bootstrap still Live after the accepted finish; must be burned")
	}
}

// TestBootstrapEnrollForwardReleaseOnPiReject: a Pi non-200 finish must RELEASE the
// reserved flag so the user can retry without the Pi re-publishing. We drive a
// finish that the Pi rejects (500), then a second finish (Pi now 200) must still
// be allowed through and succeed — proving the first reserve was rolled back.
func TestBootstrapEnrollForwardReleaseOnPiReject(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	claimKey := claimKeyHex("bootstrap-secret-release")
	var status atomic.Int32
	status.Store(http.StatusInternalServerError) // Pi rejects the first finish
	srv, seen := configurableStubPi(t, relay, hostID, &status, nil)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	resp, body := enrollPost(t, srv.URL, "/api/owner-access/enroll/finish?claim_key="+claimKey+"&pin="+pin)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("first finish: got %d (%s), want 500 (Pi rejected)", resp.StatusCode, body)
	}
	// The relay must have RELEASED, not burned — the window is still Live.
	if !relay.Bootstrap.Live(site, claimKey) {
		t.Fatal("bootstrap not Live after a Pi-rejected finish; the relay must Release (not Burn) so the user can retry")
	}
	// Retry: the Pi now accepts (200). The reserve gate must let it through.
	status.Store(http.StatusOK)
	resp2, body2 := enrollPost(t, srv.URL, "/api/owner-access/enroll/finish?claim_key="+claimKey+"&pin="+pin)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("retry finish: got %d (%s), want 200 (reserve was released)", resp2.StatusCode, body2)
	}
	if relay.Bootstrap.Live(site, claimKey) {
		t.Fatal("bootstrap still Live after the accepted retry; must be burned")
	}
	if got := seen(); len(got) != 2 {
		t.Fatalf("Pi saw %d finishes, want 2 (reject + accepted retry): %v", len(got), got)
	}
}

// TestBootstrapEnrollForwardNonHexClaimKey: a ?claim_key that is not 64-char
// lowercase hex is rejected 403 (uniform anti-enumeration) before any store
// lookup, and nothing is forwarded to the Pi.
func TestBootstrapEnrollForwardNonHexClaimKey(t *testing.T) {
	relay := newMultiTenantRelay(t)
	const site, hostID, pin = "site:A", "host-a", "123456"
	claimKey := claimKeyHex("bootstrap-secret-nonhex")
	srv, seen := enrollStubPi(t, relay, hostID)
	publishLiveBootstrap(t, relay, site, hostID, claimKey)

	for name, bad := range map[string]string{
		"too-short": "abcd",
		"not-hex":   strings.Repeat("zz", 32),
		"uppercase": strings.ToUpper(strings.Repeat("ab", 32)),
	} {
		resp, _ := enrollPost(t, srv.URL, "/api/owner-access/enroll/start?claim_key="+bad+"&pin="+pin)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s claim_key on enroll-forward: got %d, want 403", name, resp.StatusCode)
		}
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("Pi was forwarded %v on a malformed claim_key; want nothing", got)
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
	if got := resp.Header.Get("X-FTW-Error-Code"); got != errBootstrapClaimRequired {
		t.Fatalf("missing claim_key error code = %q, want %q", got, errBootstrapClaimRequired)
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
	if got := resp.Header.Get("X-FTW-Error-Code"); got != errRemoteHomeOffline {
		t.Fatalf("home offline error code = %q, want %q", got, errRemoteHomeOffline)
	}
}

func TestBootstrapEnrollForwardMissingClaimHasDiagnosticBody(t *testing.T) {
	relay := newMultiTenantRelay(t)
	srv := httptest.NewServer(relay.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/owner-access/enroll/start?pin=123456", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "home.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("diagnostic route precondition changed: got %d body=%q, want 403 from bootstrap-forward", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-FTW-Error-Code"); got != errBootstrapClaimRequired {
		t.Fatalf("diagnostic code = %q, want %q body=%q", got, errBootstrapClaimRequired, body)
	}
	if !strings.Contains(string(body), errBootstrapClaimRequired) {
		t.Fatalf("body %q does not include code %q", body, errBootstrapClaimRequired)
	}
}

func TestHomeHostRawOwnerPOSTHasP2POnlyDiagnosticCode(t *testing.T) {
	relay := newMultiTenantRelay(t)
	srv := httptest.NewServer(relay.Handler())
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "home.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("raw owner POST: got %d body=%q, want 405", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-FTW-Error-Code"); got != errRemoteP2POnly {
		t.Fatalf("raw owner POST code = %q, want %q body=%q", got, errRemoteP2POnly, body)
	}
	if !strings.Contains(string(body), errRemoteP2POnly) {
		t.Fatalf("body %q does not include code %q", body, errRemoteP2POnly)
	}
}
