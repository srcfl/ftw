package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

func newTestRelay() *Relay {
	return &Relay{
		Queue:  tunnel.NewQueue(),
		Tokens: NewTokenRegistry(),
		Owners: NewOwnerRegistry(),
	}
}

func TestHealthz(t *testing.T) {
	srv := httptest.NewServer(newTestRelay().Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestTunnelNextLongPollsAndDelivers(t *testing.T) {
	r := newTestRelay()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	respCh := make(chan tunnel.TunneledResponse, 1)
	go func() {
		resp, err := r.Queue.Enqueue(context.Background(), "host-a", tunnel.TunneledRequest{Method: "GET", Path: "/mcp"})
		if err == nil {
			respCh <- resp
		}
	}()

	resp, err := http.Get(srv.URL + "/tunnel/host-a/next")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var tr tunnel.TunneledRequest
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if tr.Path != "/mcp" {
		t.Fatalf("wrong path: %q", tr.Path)
	}

	body, _ := json.Marshal(tunnel.TunneledResponse{Status: 200, Body: []byte("ok")})
	postResp, err := http.Post(srv.URL+"/tunnel/host-a/response/"+tr.ReqID, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if postResp.StatusCode != 204 {
		t.Fatalf("post status %d", postResp.StatusCode)
	}

	select {
	case got := <-respCh:
		if got.Status != 200 || string(got.Body) != "ok" {
			t.Fatalf("wrong response: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("response never made it back to enqueuer")
	}
}

func TestTunnelNextTimesOutWith204(t *testing.T) {
	r := newTestRelay()
	r.PollTimeout = 50 * time.Millisecond
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/tunnel/host-empty/next")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestRegisterEndpoint(t *testing.T) {
	r := newTestRelay()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body := strings.NewReader(`{"host_id":"host-a","token":"tok1","ttl_ms":3600000,"approval_code":"4827","intent":"help","as":"@erik"}`)
	resp, err := http.Post(srv.URL+"/tunnel/register", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.PublicURL, "/h/tok1") {
		t.Fatalf("bad public_url: %q", out.PublicURL)
	}
}

func TestLandingPageHasCodeEntryForm(t *testing.T) {
	r := newTestRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "lookme", TTL: time.Hour,
		ApprovalCode: "4827", Intent: "help me", As: "@erik",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/h/lookme")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readBody(resp.Body)
	resp.Body.Close()
	html := string(body)

	// The approval code must NOT appear on the landing page — the
	// security model relies on the friend receiving it out-of-band
	// from the host. Showing it here would let anyone with the URL
	// activate the session.
	if strings.Contains(html, "4827") {
		t.Fatalf("approval code MUST NOT appear on the landing page:\n%s", html)
	}
	// But the input form must be there so the friend can type the
	// code they received separately.
	if !strings.Contains(html, `id="code"`) {
		t.Fatalf("code input field missing from landing page")
	}
	if !strings.Contains(html, `id="approve-form"`) {
		t.Fatalf("approve form missing")
	}
	// Identity + intent should be displayed so the friend can sanity-
	// check who they're connecting to.
	if !strings.Contains(html, "@erik") {
		t.Fatalf("identity missing from landing page")
	}
	if !strings.Contains(html, "help me") {
		t.Fatalf("intent missing from landing page")
	}
}

// TestLandingPageTokenConstMatchesPath pins the format-args wiring in
// publicLanding. Regression for the "wrong code even when right code"
// bug: a misordered fmt.Fprintf put the token *state* into the JS
// const TOKEN, so the page's approve POST hit /h/<state>/approve and
// the relay returned 403 (token-not-found mapped to forbidden), which
// the JS surfaced as "Wrong code" regardless of the code the friend
// typed.
func TestLandingPageTokenConstMatchesPath(t *testing.T) {
	r := newTestRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "alpha-beta-gamma", TTL: time.Hour,
		ApprovalCode: "1234", Intent: "intent-text", As: "@friend",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/h/alpha-beta-gamma")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readBody(resp.Body)
	resp.Body.Close()
	html := string(body)

	// The JS const that drives the approve POST must be the actual
	// token. Anything else (state, intent, as) means /h/<wrong>/approve
	// returns 403 and the friend sees "Wrong code" forever.
	if !strings.Contains(html, `const TOKEN = "alpha-beta-gamma";`) {
		t.Fatalf("landing-page JS const TOKEN is not the session token:\n%s", html)
	}
	// And the "From:" row must show the claimed identity, not the
	// token — the friend uses that to sanity-check who they're talking to.
	if !strings.Contains(html, `<p>From: <code>@friend</code></p>`) {
		t.Fatalf(`expected "From: @friend" in landing page:\n%s`, html)
	}
	if !strings.Contains(html, `<p>Intent: intent-text</p>`) {
		t.Fatalf(`expected "Intent: intent-text" in landing page:\n%s`, html)
	}
	if !strings.Contains(html, `<code id="state">pending</code>`) {
		t.Fatalf(`expected state "pending" in landing page:\n%s`, html)
	}
}

func TestApproveFlipsState(t *testing.T) {
	r := newTestRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "tok2", TTL: time.Hour, ApprovalCode: "9911",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/h/tok2/approve", "application/json", strings.NewReader(`{"code":"9911"}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	tok, _ := r.Tokens.Get("tok2")
	if tok.State() != TokenActive {
		t.Fatalf("expected active, got %v", tok.State())
	}
}

func TestPendingTokenReturns425OnPublicMCP(t *testing.T) {
	r := newTestRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "tok3", TTL: time.Hour, ApprovalCode: "1",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/h/tok3/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooEarly {
		t.Fatalf("status %d, want 425", resp.StatusCode)
	}
}
