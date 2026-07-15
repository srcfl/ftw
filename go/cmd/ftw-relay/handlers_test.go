package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/tunnel"
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
	secret := mustIssue(t, r.Polls, "host-a") // the host authenticates its polls with this

	respCh := make(chan tunnel.TunneledResponse, 1)
	go func() {
		resp, err := r.Queue.Enqueue(context.Background(), "host-a", tunnel.TunneledRequest{Method: "GET", Path: "/mcp"})
		if err == nil {
			respCh <- resp
		}
	}()

	pollReq, _ := http.NewRequest("GET", srv.URL+"/tunnel/host-a/next", nil)
	pollReq.Header.Set(tunnel.PollSecretHeader, secret)
	resp, err := http.DefaultClient.Do(pollReq)
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
	postReq, _ := http.NewRequest("POST", srv.URL+"/tunnel/host-a/response/"+tr.ReqID, bytes.NewReader(body))
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set(tunnel.PollSecretHeader, secret)
	postResp, err := http.DefaultClient.Do(postReq)
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
	pollReq, _ := http.NewRequest("GET", srv.URL+"/tunnel/host-empty/next", nil)
	pollReq.Header.Set(tunnel.PollSecretHeader, mustIssue(t, r.Polls, "host-empty"))
	resp, err := http.DefaultClient.Do(pollReq)
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

// TestRegisterRejectsUnsafeToken proves /tunnel/register — open to the
// internet — refuses a token carrying HTML/JS metacharacters, so it can never
// be planted and reflected into the landing page (the XSS sink).
func TestRegisterRejectsUnsafeToken(t *testing.T) {
	r := newTestRelay()
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body := strings.NewReader(`{"host_id":"host-a","token":"</script><script>alert(1)</script>","ttl_ms":3600000,"approval_code":"4827"}`)
	resp, err := http.Post(srv.URL+"/tunnel/register", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsafe token must be rejected with 400, got %d", resp.StatusCode)
	}
}

// TestLandingPageJSONEncodesToken is the defence-in-depth layer: even if a
// token with a quote/script sequence reaches the registry directly (bypassing
// the register-time charset guard), publicLanding must \u-escape it via
// json.Marshal so it cannot break out of the <script> context.
func TestLandingPageJSONEncodesToken(t *testing.T) {
	r := newTestRelay()
	const nasty = `a</script><script>x`
	// Register straight into the registry (no HTTP validation) to simulate a
	// hypothetical bypass of validPairToken.
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: nasty, TTL: time.Hour, ApprovalCode: "1234",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/h/" + url.PathEscape(nasty))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := readBody(resp.Body)
	resp.Body.Close()
	html := string(body)
	// The raw closing tag must NOT appear inside the script literal.
	if strings.Contains(html, `const TOKEN = "a</script>`) {
		t.Fatalf("token broke out of the <script> context:\n%s", html)
	}
	// Reconstruct exactly what publicLanding emits: the token const must be the
	// json.Marshal (angle-bracket-escaped) form, not the raw breakout sequence.
	escaped, _ := json.Marshal(nasty)
	if !strings.Contains(html, "const TOKEN = "+string(escaped)+";") {
		t.Fatalf("token const is not the json-escaped form:\n%s", html)
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
	// Approve now returns 200 + the minted grant (was 204 before the
	// grant-exchange model).
	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var out struct {
		Grant string `json:"grant"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Grant == "" {
		t.Fatal("approve did not return a grant")
	}
	tok, _ := r.Tokens.Get("tok2")
	if tok.State() != TokenActive {
		t.Fatalf("expected active, got %v", tok.State())
	}
}

func TestPublicMCPRequiresGrant(t *testing.T) {
	r := newTestRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "tok3", TTL: time.Hour, ApprovalCode: "1",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	// Pending (no grant minted yet) → 401.
	resp, err := http.Post(srv.URL+"/h/tok3/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pending /mcp status %d, want 401", resp.StatusCode)
	}
	// Activate, then a wrong Bearer is still rejected.
	_ = r.Tokens.Approve("tok3", "1")
	bad, _ := http.NewRequest("POST", srv.URL+"/h/tok3/mcp", strings.NewReader(`{}`))
	bad.Header.Set("Authorization", "Bearer not-the-grant")
	bresp, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	bresp.Body.Close()
	if bresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-Bearer /mcp status %d, want 401", bresp.StatusCode)
	}
}
