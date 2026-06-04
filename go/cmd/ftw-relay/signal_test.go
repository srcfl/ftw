package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	host.SetPollSecret(relay.Polls.Issue("host-home"))
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
