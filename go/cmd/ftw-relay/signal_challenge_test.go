package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// signal_challenge_test.go — C2: the device-key proof on the signaling offer. A
// browser must (1) GET a single-use challenge nonce, (2) sign
// "ftw-signal:v1:<site>:<nonce>" with a device key the Pi published (C1), and (3)
// POST {device_pubkey,nonce,sig} on the offer. Only then does the relay forward
// the SDP to the Pi. Every failure → 403 and the Pi is NEVER contacted.

// TestSignalChallenge_NonceLifecycle proves the challenge store: a freshly issued
// nonce verifies once and is consumed (single-use); a replayed, unknown, or
// expired nonce is refused.
func TestSignalChallenge_NonceLifecycle(t *testing.T) {
	c := NewSignalChallenges()
	const site = "site:Home"

	nonce, expMs, ok := c.Issue(site)
	if !ok || nonce == "" {
		t.Fatalf("Issue returned ok=%v nonce=%q", ok, nonce)
	}
	if expMs <= time.Now().UnixMilli() {
		t.Fatalf("exp_ms %d is not in the future", expMs)
	}
	// First consume succeeds.
	if !c.Consume(site, nonce) {
		t.Fatal("a fresh nonce must consume once")
	}
	// REPLAY: second consume of the same nonce fails (single-use).
	if c.Consume(site, nonce) {
		t.Fatal("a consumed nonce must NOT consume again (replay)")
	}
	// Unknown nonce → false.
	if c.Consume(site, "bogus-nonce") {
		t.Fatal("an unknown nonce must NOT consume")
	}
	// Unknown site → false.
	other, _, _ := c.Issue("site:Other")
	if c.Consume("site:Nope", other) {
		t.Fatal("a nonce from a different site must NOT consume")
	}
	// Empty nonce → false.
	if c.Consume(site, "") {
		t.Fatal("an empty nonce must NOT consume")
	}
}

// TestSignalChallenge_Expiry proves an expired nonce is refused even though it
// was a real issued nonce.
func TestSignalChallenge_Expiry(t *testing.T) {
	c := NewSignalChallenges()
	const site = "site:Home"
	nonce, _, _ := c.Issue(site)
	// Force the stored nonce to be already expired by reaching into the store
	// (same package). This is the only way to test expiry without sleeping 60s.
	c.mu.Lock()
	c.bySite[site][nonce] = signalChallenge{exp: time.Now().Add(-time.Second)}
	c.mu.Unlock()
	if c.Consume(site, nonce) {
		t.Fatal("an expired nonce must NOT consume")
	}
}

// TestSignalChallenge_GCReapsExpired proves GC trims expired nonces and empty
// sites.
func TestSignalChallenge_GCReapsExpired(t *testing.T) {
	c := NewSignalChallenges()
	n1, _, _ := c.Issue("site:A")
	c.Issue("site:B")
	// Expire site:A's nonce.
	c.mu.Lock()
	c.bySite["site:A"][n1] = signalChallenge{exp: time.Now().Add(-time.Minute)}
	c.mu.Unlock()
	if removed := c.GC(0); removed < 1 {
		t.Fatalf("GC removed %d, want >= 1 expired nonce", removed)
	}
	// site:A is now empty and gone; site:B's fresh nonce survives.
	c.mu.Lock()
	_, aGone := c.bySite["site:A"]
	_, bAlive := c.bySite["site:B"]
	c.mu.Unlock()
	if aGone {
		t.Fatal("expired+empty site must be reaped")
	}
	if !bAlive {
		t.Fatal("a site with a fresh nonce must survive GC")
	}
}

// c2Relay bundles a C2 test relay: a live server, the relay, the trusted device
// key, and the Pi's poll secret.
type c2Relay struct {
	ts     *httptest.Server
	r      *Relay
	dev    *deviceKey
	secret string
}

// newC2Relay builds a relay with a registered site, a trusted device key, and the
// challenge store. Tests drive the Pi drain SYNCHRONOUSLY via drainOffer (which
// long-polls once for an ALREADY-parked offer, returning at once) rather than
// spinning a background poller — that keeps the tests fast and avoids stranding a
// 25s server-side long-poll at server Close.
func newC2Relay(t *testing.T) *c2Relay {
	t.Helper()
	owners := NewOwnerRegistry()
	if err := owners.Register("site:Home", "host-xyz", "deadbeef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	dev := newDeviceKey(t)
	owners.SetDeviceKeys("site:Home", []string{dev.pubKeyHex})
	r := &Relay{
		Owners:      owners,
		Polls:       NewPollSecrets(),
		Signals:     NewSignalMailbox(),
		Challenges:  NewSignalChallenges(),
		PollTimeout: 200 * time.Millisecond,
	}
	secret := mustIssue(t, r.Polls, "host-xyz")
	ts := httptest.NewServer(r.Handler())
	t.Cleanup(ts.Close)
	return &c2Relay{ts: ts, r: r, dev: dev, secret: secret}
}

// drainOfferNow consumes any ALREADY-parked offer for the site WITHOUT blocking:
// it calls the mailbox in-process with a zero timeout, so the no-offer case
// returns at once (rather than stranding a 25s server-side long-poll that would
// then block server Close). Returns the raw SDP the Pi would have drained. This
// exercises exactly what signalHostOffer forwards (the relay parks only the raw
// SDP under C2), so it is a faithful "did the Pi get contacted?" probe.
func (c *c2Relay) drainOfferNow(t *testing.T) (string, bool) {
	t.Helper()
	data, _, ok := c.r.Signals.TakeOffer("site:Home", 0)
	if !ok {
		return "", false
	}
	return string(data), true
}

// postOffer posts a C2 offer envelope and returns the status code.
func postOffer(t *testing.T, baseURL string, body []byte) int {
	t.Helper()
	resp, err := http.Post(baseURL+"/signal/"+urlSite("site:Home")+"/offer?n=00112233aabbccdd",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post offer: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// challengeFor mints a challenge nonce via the live relay handler.
func challengeFor(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/signal/" + urlSite("site:Home") + "/challenge")
	if err != nil {
		t.Fatalf("challenge: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Nonce string `json:"nonce"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Nonce
}

// TestSignalOffer_ValidProof_Forwards proves the happy path: a correctly signed
// offer is accepted (204) and the SDP reaches the Pi.
func TestSignalOffer_ValidProof_Forwards(t *testing.T) {
	c := newC2Relay(t)
	nonce := challengeFor(t, c.ts.URL)
	body := offerEnvelope(t, c.dev, "site:Home", nonce, "REAL-SDP")
	if code := postOffer(t, c.ts.URL, body); code != http.StatusNoContent {
		t.Fatalf("valid offer status = %d, want 204", code)
	}
	// The offer is parked; the Pi drains it at once and sees the raw SDP.
	sdp, ok := c.drainOfferNow(t)
	if !ok {
		t.Fatal("valid offer never reached the Pi")
	}
	if sdp != "REAL-SDP" {
		t.Fatalf("Pi drained %q, want REAL-SDP", sdp)
	}
}

// TestSignalOffer_UnknownPubkey_403_PiNeverContacted proves a device_pubkey NOT
// in the site's published set is refused with 403 and the Pi is never contacted
// (nothing is parked for it to drain).
func TestSignalOffer_UnknownPubkey_403_PiNeverContacted(t *testing.T) {
	c := newC2Relay(t)
	stranger := newDeviceKey(t) // a key the Pi never published
	nonce := challengeFor(t, c.ts.URL)
	body := offerEnvelope(t, stranger, "site:Home", nonce, "EVIL-SDP")
	if code := postOffer(t, c.ts.URL, body); code != http.StatusForbidden {
		t.Fatalf("unknown-pubkey offer status = %d, want 403", code)
	}
	if sdp, ok := c.drainOfferNow(t); ok {
		t.Fatalf("Pi was contacted on an unknown-pubkey offer: %q", sdp)
	}
}

// TestSignalOffer_BadSig_403 proves a malformed/forged signature is refused even
// when the device_pubkey IS trusted and the nonce IS fresh, and nothing reaches
// the Pi.
func TestSignalOffer_BadSig_403(t *testing.T) {
	c := newC2Relay(t)
	nonce := challengeFor(t, c.ts.URL)
	// Build a valid envelope, then corrupt the signature.
	body := offerEnvelope(t, c.dev, "site:Home", nonce, "SDP")
	var env map[string]string
	_ = json.Unmarshal(body, &env)
	env["sig"] = "AAAA" // wrong length / not a real sig
	bad, _ := json.Marshal(env)
	if code := postOffer(t, c.ts.URL, bad); code != http.StatusForbidden {
		t.Fatalf("bad-sig offer status = %d, want 403", code)
	}

	// A signature for a DIFFERENT nonce (lifted from another challenge) must also
	// fail against this nonce.
	other := challengeFor(t, c.ts.URL)
	lifted := offerEnvelope(t, c.dev, "site:Home", other, "SDP") // signs `other`
	var lenv map[string]string
	_ = json.Unmarshal(lifted, &lenv)
	lenv["nonce"] = nonce // ...but claims `nonce`
	mismatched, _ := json.Marshal(lenv)
	if code := postOffer(t, c.ts.URL, mismatched); code != http.StatusForbidden {
		t.Fatalf("nonce/sig-mismatch offer status = %d, want 403", code)
	}
	if _, ok := c.drainOfferNow(t); ok {
		t.Fatal("a bad-sig offer must NOT reach the Pi")
	}
}

// TestSignalOffer_ReplayedNonce_403 proves a (device_pubkey,nonce,sig) triple
// that already succeeded can't be replayed: the nonce was consumed on first use.
func TestSignalOffer_ReplayedNonce_403(t *testing.T) {
	c := newC2Relay(t)
	nonce := challengeFor(t, c.ts.URL)
	body := offerEnvelope(t, c.dev, "site:Home", nonce, "SDP")
	if code := postOffer(t, c.ts.URL, body); code != http.StatusNoContent {
		t.Fatalf("first offer status = %d, want 204", code)
	}
	// Drain the first (legitimate) offer so the mailbox is empty for the replay.
	if _, ok := c.drainOfferNow(t); !ok {
		t.Fatal("first valid offer should have reached the Pi")
	}
	// Replay the exact same body — the nonce is gone, so it must be refused.
	if code := postOffer(t, c.ts.URL, body); code != http.StatusForbidden {
		t.Fatalf("replayed offer status = %d, want 403", code)
	}
}

// TestSignalOffer_NoDeviceKeysPublished_403 proves fail-closed: a site that
// published NO device keys (C1 never ran) accepts NO device-key signaling — every
// offer is 403, the Pi is never reachable that way.
func TestSignalOffer_NoDeviceKeysPublished_403(t *testing.T) {
	owners := NewOwnerRegistry()
	if err := owners.Register("site:Home", "host-xyz", "deadbeef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Deliberately DO NOT publish any device keys.
	r := &Relay{
		Owners:      owners,
		Polls:       NewPollSecrets(),
		Signals:     NewSignalMailbox(),
		Challenges:  NewSignalChallenges(),
		PollTimeout: 100 * time.Millisecond,
	}
	ts := httptest.NewServer(r.Handler())
	t.Cleanup(ts.Close)

	dev := newDeviceKey(t)
	nonce := challengeFor(t, ts.URL)
	body := offerEnvelope(t, dev, "site:Home", nonce, "SDP")
	if code := postOffer(t, ts.URL, body); code != http.StatusForbidden {
		t.Fatalf("offer with no published device keys status = %d, want 403", code)
	}
}
