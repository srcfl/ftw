package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// newSignalRelay builds a relay with a registered site->host mapping and the
// minted poll secret, returning the test server, host_id, site_id, and secret.
func newSignalRelay(t *testing.T) (*httptest.Server, *Relay, string) {
	t.Helper()
	owners := NewOwnerRegistry()
	// Bind site:Home -> host-xyz so SiteForHost resolves on the Pi side.
	if err := owners.Register("site:Home", "host-xyz", "deadbeef"); err != nil {
		t.Fatalf("register: %v", err)
	}
	r := &Relay{
		Owners:      owners,
		Polls:       NewPollSecrets(),
		Signals:     NewSignalMailbox(),
		PollTimeout: time.Second,
	}
	secret := r.Polls.Issue("host-xyz")
	ts := httptest.NewServer(r.Handler())
	t.Cleanup(ts.Close)
	return ts, r, secret
}

// TestSignalRendezvous_OfferAnswer drives the full blind exchange: a browser
// parks an offer, the Pi (with the poll secret) drains it and parks an answer,
// the browser long-polls the answer back. Proves the relay forwards opaque
// blobs verbatim and the host_id->site_id mapping wires the two ends together.
func TestSignalRendezvous_OfferAnswer(t *testing.T) {
	ts, _, secret := newSignalRelay(t)

	// 1. Browser parks an offer for the site.
	resp, err := http.Post(ts.URL+"/signal/"+urlSite("site:Home")+"/offer", "application/json",
		bytes.NewReader([]byte("OFFER-SDP")))
	if err != nil {
		t.Fatalf("park offer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("park offer status = %d, want 204", resp.StatusCode)
	}

	// 2. Pi drains the offer with its poll secret (host-keyed path).
	off := mustGet(t, ts.URL+"/signal/host-xyz/offer", secret)
	if string(off) != "OFFER-SDP" {
		t.Fatalf("drained offer = %q, want OFFER-SDP", off)
	}

	// 3. Pi parks an answer blob.
	areq, _ := http.NewRequest(http.MethodPost, ts.URL+"/signal/host-xyz/answer",
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

	// 4. Browser long-polls the answer back.
	ans := mustGet(t, ts.URL+"/signal/"+urlSite("site:Home")+"/answer", "")
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

// TestSignalOffer_RateLimited proves a second offer within the min interval is
// rejected with 429.
func TestSignalOffer_RateLimited(t *testing.T) {
	ts, _, _ := newSignalRelay(t)
	url := ts.URL + "/signal/" + urlSite("site:Home") + "/offer"
	r1, err := http.Post(url, "application/json", bytes.NewReader([]byte("A")))
	if err != nil {
		t.Fatalf("post1: %v", err)
	}
	r1.Body.Close()
	if r1.StatusCode != http.StatusNoContent {
		t.Fatalf("first offer = %d, want 204", r1.StatusCode)
	}
	r2, err := http.Post(url, "application/json", bytes.NewReader([]byte("B")))
	if err != nil {
		t.Fatalf("post2: %v", err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("rapid second offer = %d, want 429", r2.StatusCode)
	}
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
