package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// newSignalRelay builds a relay with a registered site->host mapping, a trusted
// device key (C2), and the minted poll secret, returning the test server, the
// relay, and the poll secret. The trusted device key is reachable via
// r.Owners.HasDeviceKey; tests that need to sign an offer use signedOffer.
func newSignalRelay(t *testing.T) (*httptest.Server, *Relay, string) {
	t.Helper()
	owners := NewOwnerRegistry()
	// Bind site:Home -> host-xyz so SiteForHost resolves on the Pi side.
	if err := owners.Register("site:Home", "host-xyz", "deadbeef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Publish a trusted device key for the site so the C2 offer proof can pass.
	owners.SetDeviceKeys("site:Home", []string{testDeviceKey.pubKeyHex})
	r := &Relay{
		Owners:           owners,
		Polls:            NewPollSecrets(),
		Signals:          NewSignalMailbox(),
		Challenges:       NewSignalChallenges(),
		PollTimeout:      time.Second,
		RequireDeviceKey: true,
	}
	secret := mustIssue(t, r.Polls, "host-xyz")
	ts := httptest.NewServer(r.Handler())
	t.Cleanup(ts.Close)
	return ts, r, secret
}

// testDeviceKey is a process-wide trusted device key for the signaling tests. It
// is registered into newSignalRelay's owner registry and used by signedOffer to
// build a valid C2 proof. (One shared key keeps the tests terse; the proof path
// is exercised against a real P-256 keypair.)
var testDeviceKey = mustGenDeviceKey()

func mustGenDeviceKey() *deviceKey {
	priv, err := genP256()
	if err != nil {
		panic("signal_test: gen device key: " + err.Error())
	}
	return &deviceKey{priv: priv, pubKeyHex: devicePubKeyHex(priv)}
}

// signedOffer fetches a fresh challenge nonce from the relay, signs it with the
// trusted test device key, and POSTs the C2 offer envelope (raw SDP + proof)
// under the given rendezvous nonce. It returns the HTTP status. This is what a
// real browser does, so every signaling test drives the full proof path.
func signedOffer(t *testing.T, baseURL, siteID, rendezvousNonce, sdp string) int {
	t.Helper()
	challenge := fetchChallenge(t, baseURL, siteID)
	body := offerEnvelope(t, testDeviceKey, siteID, challenge, sdp)
	resp, err := http.Post(baseURL+"/signal/"+urlSite(siteID)+"/offer?n="+rendezvousNonce,
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post signed offer: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// fetchChallenge GETs a single-use challenge nonce for siteID from the relay.
func fetchChallenge(t *testing.T, baseURL, siteID string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/signal/" + urlSite(siteID) + "/challenge")
	if err != nil {
		t.Fatalf("get challenge: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("challenge status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Nonce string `json:"nonce"`
		ExpMs int64  `json:"exp_ms"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	if out.Nonce == "" {
		t.Fatal("challenge returned an empty nonce")
	}
	return out.Nonce
}

// TestSignalRendezvous_OfferAnswer drives the full blind exchange: a browser
// parks an offer, the Pi (with the poll secret) drains it and parks an answer,
// the browser long-polls the answer back. Proves the relay forwards opaque
// blobs verbatim and the host_id->site_id mapping wires the two ends together.
func TestSignalRendezvous_OfferAnswer(t *testing.T) {
	ts, _, secret := newSignalRelay(t)
	const nonce = "00112233445566778899aabbccddeeff"

	// 1. Browser parks a SIGNED offer (C2 device proof) for the site under its
	//    rendezvous nonce.
	if code := signedOffer(t, ts.URL, "site:Home", nonce, "OFFER-SDP"); code != http.StatusNoContent {
		t.Fatalf("park offer status = %d, want 204", code)
	}

	// 2. Pi drains the offer with its poll secret (host-keyed path) and gets the
	//    nonce echoed back in the response header.
	off, drainNonce := mustGetWithNonce(t, ts.URL+"/signal/host-xyz/offer", secret)
	if string(off) != "OFFER-SDP" {
		t.Fatalf("drained offer = %q, want OFFER-SDP", off)
	}
	if drainNonce != nonce {
		t.Fatalf("drained nonce = %q, want %q", drainNonce, nonce)
	}

	// 3. Pi parks an answer blob under the echoed nonce.
	areq, _ := http.NewRequest(http.MethodPost, ts.URL+"/signal/host-xyz/answer?n="+drainNonce,
		bytes.NewReader([]byte(`{"sdp":"ANSWER-SDP","fp_sig":"sig","ts":1}`)))
	areq.Header.Set(tunnel.PollSecretHeader, secret)
	ar, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatalf("park answer: %v", err)
	}
	ar.Body.Close()
	if ar.StatusCode != http.StatusNoContent {
		t.Fatalf("park answer status = %d, want 204", ar.StatusCode)
	}

	// 4. Browser long-polls the answer back on its own nonce.
	ans := mustGet(t, ts.URL+"/signal/"+urlSite("site:Home")+"/answer?n="+nonce, "")
	if !bytes.Contains(ans, []byte("ANSWER-SDP")) {
		t.Fatalf("answer = %q, want it to contain ANSWER-SDP", ans)
	}
}

// TestSignalHostOffer_RequiresPollSecret proves the Pi-side drain is
// authenticated: a caller knowing only host_id but not the poll secret gets 401.
func TestSignalHostOffer_RequiresPollSecret(t *testing.T) {
	ts, _, _ := newSignalRelay(t)
	// No X-FTW-Poll header.
	resp, err := http.Get(ts.URL + "/signal/host-xyz/offer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated host offer poll = %d, want 401", resp.StatusCode)
	}

	// Wrong secret also 401.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/signal/host-xyz/offer", nil)
	req.Header.Set(tunnel.PollSecretHeader, "wrong")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-secret host offer poll = %d, want 401", resp2.StatusCode)
	}
}

// TestSignalHostAnswer_RequiresPollSecret proves the Pi-side answer-park is
// authenticated too.
func TestSignalHostAnswer_RequiresPollSecret(t *testing.T) {
	ts, _, _ := newSignalRelay(t)
	resp, err := http.Post(ts.URL+"/signal/host-xyz/answer", "application/json",
		bytes.NewReader([]byte(`{"sdp":"x"}`)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated host answer = %d, want 401", resp.StatusCode)
	}
}

// TestSignalOffer_RateLimited proves a sustained burst of offers from one source
// eventually hits 429 (the per-IP primary throttle), while a small burst — what a
// legit browser does on a flaky network — is allowed. The old per-site 500ms
// min-interval (429 on the immediate SECOND offer) was a lockout lever and is
// gone; see TestSignalOffer_PerIP_NoCrossLockout for the lever-removal guard.
func TestSignalOffer_RateLimited(t *testing.T) {
	ts, _, _ := newSignalRelay(t)
	post := func(nonce string) int {
		return signedOffer(t, ts.URL, "site:Home", nonce, "A")
	}
	// A few rapid offers (within the burst) are accepted — a legit retrying browser
	// is never blocked on its first attempts.
	if code := post("aabbccddeeff0011"); code != http.StatusNoContent {
		t.Fatalf("first offer = %d, want 204", code)
	}
	if code := post("aabbccddeeff0012"); code != http.StatusNoContent {
		t.Fatalf("second offer (within burst) = %d, want 204", code)
	}
	// Drive a sustained flood from the same source; it MUST eventually 429 (per-IP
	// bound). The per-IP burst is offerBucketCapacity, so well within a small loop.
	got429 := false
	for i := 0; i < int(offerBucketCapacity)+8; i++ {
		nonce := "ccddeeff0011" + fmtNonceSuffix(i)
		if post(nonce) == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("a sustained offer flood from one source was never throttled (per-IP limit missing)")
	}
}

// fmtNonceSuffix returns a 4-hex-digit suffix so each offer uses a distinct
// (bounded-charset) nonce.
func fmtNonceSuffix(i int) string {
	const hexd = "0123456789abcdef"
	return string([]byte{
		hexd[(i>>12)&0xf], hexd[(i>>8)&0xf], hexd[(i>>4)&0xf], hexd[i&0xf],
	})
}

func mustGet(t *testing.T, url, pollSecret string) []byte {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if pollSecret != "" {
		req.Header.Set(tunnel.PollSecretHeader, pollSecret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s status = %d, want 200", url, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return b
}

// mustGetWithNonce is mustGet that also returns the echoed X-FTW-Signal-Nonce
// header (the Pi's offer-drain returns the browser's nonce so the answer routes
// back to the right per-nonce mailbox).
func mustGetWithNonce(t *testing.T, url, pollSecret string) ([]byte, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if pollSecret != "" {
		req.Header.Set(tunnel.PollSecretHeader, pollSecret)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s status = %d, want 200", url, resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	return b, resp.Header.Get("X-FTW-Signal-Nonce")
}

// urlSite percent-encodes a site_id for a path segment (the colon in
// "site:Home" must be encoded so it lands as a single PathValue).
func urlSite(s string) string {
	out := ""
	for _, c := range []byte(s) {
		if c == ':' {
			out += "%3A"
			continue
		}
		out += string(c)
	}
	return out
}

// TestHomeStaticForward_FailClosed is the slice-6 guard for the home-host
// static forwarder: GET of a non-/api/ asset (and the single /api/identity TOFU
// exception) reaches the Pi, but the owner API (/api/*) and any non-GET method
// are refused at the relay so no owner request/cookie ever traverses it.
func TestHomeStaticForward_FailClosed(t *testing.T) {
	owners := NewOwnerRegistry()
	if err := owners.Register("site:e2e", "host-home", "deadbeef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		Owners:      owners,
		Polls:       NewPollSecrets(),
		Signals:     NewSignalMailbox(),
		PollTimeout: time.Second,
		HomeHost:    "home.test",
		HomeSite:    "site:e2e",
	}
	srv := httptest.NewServer(relay.Handler())
	t.Cleanup(srv.Close)

	// Fake Pi serving any forwarded asset; it also sets an owner cookie + echoes
	// any inbound Cookie so we can prove the relay strips both directions.
	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Inner-Path", r.URL.Path)
		w.Header().Set("X-Saw-Cookie", r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "ftw_owner", Value: "leak", Path: "/"})
		_, _ = w.Write([]byte("asset:" + r.URL.Path))
	})
	host := tunnel.NewHost(srv.URL, "host-home", backend)
	host.PollTimeout = time.Second
	host.SetPollSecret(mustIssue(t, relay.Polls, "host-home"))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go host.Run(ctx)

	homeGet := func(path string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Host = "home.test"
		// A browser might still carry a stale owner cookie; the relay must strip it.
		req.AddCookie(&http.Cookie{Name: "ftw_owner", Value: "stale"})
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		return resp
	}

	// 1. Static asset GET → forwarded; relay strips the inbound owner cookie and
	//    the outbound Set-Cookie.
	r1 := homeGet("/index.html")
	defer r1.Body.Close()
	if r1.StatusCode != 200 {
		t.Fatalf("static GET status=%d, want 200", r1.StatusCode)
	}
	if r1.Header.Get("X-Saw-Cookie") != "" {
		t.Fatalf("relay forwarded an owner cookie to the Pi: %q", r1.Header.Get("X-Saw-Cookie"))
	}
	for _, sc := range r1.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, "ftw_owner=") {
			t.Fatalf("relay leaked an owner Set-Cookie to the browser: %q", sc)
		}
	}

	// 2. /api/identity GET → allowed (TOFU anchor, public key only).
	r2 := homeGet("/api/identity")
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("/api/identity status=%d, want 200 (TOFU exception)", r2.StatusCode)
	}

	// 3. Any other /api/* GET → refused at the relay (403), never forwarded.
	r3 := homeGet("/api/owner-access/whoami")
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusForbidden {
		t.Fatalf("/api/owner-access/whoami over relay status=%d, want 403 (P2P-only)", r3.StatusCode)
	}

	// 4. A non-GET (e.g. login POST) → refused with 405, never forwarded.
	preq, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/owner-access/login/finish", nil)
	preq.Host = "home.test"
	r4, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer r4.Body.Close()
	if r4.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("owner POST over relay status=%d, want 405 (P2P-only)", r4.StatusCode)
	}
}

// TestSignalOffer_RolloutGateOff proves the device-key gate is DORMANT by default
// (-require-device-key off): a raw-SDP offer with NO device-key proof is parked
// and forwarded verbatim, so the relay can serve the shell + identity (slices
// 1+2) while a home Pi that doesn't yet publish device-keys keeps working.
func TestSignalOffer_RolloutGateOff(t *testing.T) {
	owners := NewOwnerRegistry()
	if err := owners.Register("site:Home", "host-xyz", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	r := &Relay{
		Owners:      owners,
		Polls:       NewPollSecrets(),
		Signals:     NewSignalMailbox(),
		Challenges:  NewSignalChallenges(),
		PollTimeout: time.Second,
		// RequireDeviceKey defaults to false — the rollout default.
	}
	secret := mustIssue(t, r.Polls, "host-xyz")
	ts := httptest.NewServer(r.Handler())
	defer ts.Close()
	const nonce = "00112233445566778899aabbccddeeff"
	// Raw SDP, no envelope, no device-key proof.
	resp, err := http.Post(ts.URL+"/signal/site:Home/offer?n="+nonce, "application/sdp", strings.NewReader("RAW-OFFER-SDP"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rollout-off offer status = %d, want 204 (gate dormant)", resp.StatusCode)
	}
	off, _ := mustGetWithNonce(t, ts.URL+"/signal/host-xyz/offer", secret)
	if string(off) != "RAW-OFFER-SDP" {
		t.Fatalf("drained offer = %q, want RAW-OFFER-SDP parked verbatim", off)
	}
}

func TestSignalOffer_RolloutGateOffUnwrapsProofEnvelope(t *testing.T) {
	owners := NewOwnerRegistry()
	if err := owners.Register("site:Home", "host-xyz", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	r := &Relay{
		Owners:      owners,
		Polls:       NewPollSecrets(),
		Signals:     NewSignalMailbox(),
		Challenges:  NewSignalChallenges(),
		PollTimeout: time.Second,
	}
	secret := mustIssue(t, r.Polls, "host-xyz")
	ts := httptest.NewServer(r.Handler())
	defer ts.Close()
	const nonce = "00112233445566778899aabbccddeeff"

	body := `{"sdp":"RAW-OFFER-SDP","device_pubkey":"ignored","nonce":"ignored","sig":"ignored"}`
	resp, err := http.Post(ts.URL+"/signal/site:Home/offer?n="+nonce, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("rollout-off envelope offer status = %d, want 204", resp.StatusCode)
	}
	off, _ := mustGetWithNonce(t, ts.URL+"/signal/host-xyz/offer", secret)
	if string(off) != "RAW-OFFER-SDP" {
		t.Fatalf("drained offer = %q, want raw SDP unwrapped from envelope", off)
	}
}
