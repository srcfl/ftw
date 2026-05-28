package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// newSubdomainRelay is a relay with Host routing enabled for tests.
func newSubdomainRelay() *Relay {
	r := newTestRelay()
	r.BaseDomain = "fortytwowatts.com"
	return r
}

func TestSessionTokenParser(t *testing.T) {
	r := newSubdomainRelay()
	cases := []struct {
		host    string
		wantTok string
		wantOK  bool
		blurb   string
	}{
		{"amber-beam-drift.fortytwowatts.com", "amber-beam-drift", true, "plain subdomain"},
		{"amber-beam-drift.fortytwowatts.com:443", "amber-beam-drift", true, "strips port"},
		{"Amber-Beam-Drift.FortyTwoWatts.com", "amber-beam-drift", true, "lowercases"},
		{"amber-beam-drift.fortytwowatts.com.", "amber-beam-drift", true, "tolerates trailing dot"},
		{"relay.fortytwowatts.com", "", false, "reserved control-plane label"},
		{"www.fortytwowatts.com", "", false, "reserved www"},
		{"subetha.fortytwowatts.com", "", false, "reserved legacy label"},
		{"fortytwowatts.com", "", false, "apex is not a session"},
		{"a.b.fortytwowatts.com", "", false, "multi-level outside the flat scheme"},
		{"127.0.0.1:8080", "", false, "raw IP"},
		{"localhost:7378", "", false, "localhost dev"},
		{"evil.com", "", false, "foreign domain"},
		{"fortytwowatts.com.evil.com", "", false, "suffix-spoof"},
	}
	for _, c := range cases {
		gotTok, gotOK := r.sessionToken(c.host)
		if gotTok != c.wantTok || gotOK != c.wantOK {
			t.Errorf("sessionToken(%q) = (%q, %v), want (%q, %v) — %s",
				c.host, gotTok, gotOK, c.wantTok, c.wantOK, c.blurb)
		}
	}
}

func TestSessionTokenDisabledWhenNoBaseDomain(t *testing.T) {
	r := newTestRelay() // BaseDomain == ""
	if _, ok := r.sessionToken("anything.fortytwowatts.com"); ok {
		t.Fatal("Host routing must be off when BaseDomain is empty")
	}
}

// get issues a GET with an explicit Host header against the test server.
func reqWithHost(t *testing.T, srv *httptest.Server, method, path, host string, body string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host // what the handler reads as req.Host
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSubdomainLandingWhenPending(t *testing.T) {
	r := newSubdomainRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "amber-beam", TTL: time.Hour,
		ApprovalCode: "4827", Intent: "help me", As: "@erik",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp := reqWithHost(t, srv, "GET", "/", "amber-beam.fortytwowatts.com", "")
	body, _ := readBody(resp.Body)
	resp.Body.Close()
	html := string(body)

	if resp.StatusCode != 200 {
		t.Fatalf("status %d, want 200\n%s", resp.StatusCode, html)
	}
	if !strings.Contains(html, `id="approve-form"`) {
		t.Fatalf("landing form missing on subdomain:\n%s", html)
	}
	// Subdomain mode posts to the session-root /approve, never /h/<token>/.
	if !strings.Contains(html, `const APPROVE_PATH = "/approve";`) {
		t.Fatalf("APPROVE_PATH should be /approve on a subdomain:\n%s", html)
	}
	if !strings.Contains(html, `const DASHBOARD_PATH = "/";`) {
		t.Fatalf("DASHBOARD_PATH should be / on a subdomain:\n%s", html)
	}
	if strings.Contains(html, "/h/amber-beam") {
		t.Fatalf("subdomain landing must not reference path-mode /h/<token> URLs:\n%s", html)
	}
	// The code must never appear on the page.
	if strings.Contains(html, "4827") {
		t.Fatalf("approval code leaked onto subdomain landing page:\n%s", html)
	}
}

func TestSubdomainApproveFlipsState(t *testing.T) {
	r := newSubdomainRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "amber-beam", TTL: time.Hour, ApprovalCode: "4827",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp := reqWithHost(t, srv, "POST", "/approve", "amber-beam.fortytwowatts.com", `{"code":"4827"}`)
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("approve status %d, want 204", resp.StatusCode)
	}
	tok, _ := r.Tokens.Get("amber-beam")
	if tok.State() != TokenActive {
		t.Fatalf("token not active after approve: %v", tok.State())
	}
}

func TestSubdomainPendingNonRootReturns425(t *testing.T) {
	r := newSubdomainRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "amber-beam", TTL: time.Hour, ApprovalCode: "4827",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	resp := reqWithHost(t, srv, "GET", "/api/status", "amber-beam.fortytwowatts.com", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooEarly {
		t.Fatalf("pending non-root status %d, want 425", resp.StatusCode)
	}
}

func TestSubdomainTunnelsVerbatimWhenActive(t *testing.T) {
	r := newSubdomainRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "amber-beam", TTL: time.Hour, ApprovalCode: "4827",
	})
	_ = r.Tokens.Approve("amber-beam", "4827")
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	// Fake host: pull the tunneled request off the queue and answer it,
	// echoing the path the relay forwarded so we can assert verbatim
	// (no prefix stripping) + query preservation.
	go func() {
		tr, err := r.Queue.Poll(context.Background(), "host-a", 2*time.Second)
		if err != nil {
			return
		}
		out, _ := json.Marshal(map[string]string{"saw_path": tr.Path, "saw_method": tr.Method})
		_ = r.Queue.PostResponse(tr.ReqID, tunnel.TunneledResponse{
			Status: 200,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body:   out,
		})
	}()

	resp := reqWithHost(t, srv, "GET", "/api/history?range=24h&points=288", "amber-beam.fortytwowatts.com", "")
	body, _ := readBody(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("active tunnel status %d, want 200\n%s", resp.StatusCode, body)
	}
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode echo: %v (%s)", err, body)
	}
	if got["saw_path"] != "/api/history?range=24h&points=288" {
		t.Fatalf("host saw path %q, want verbatim /api/history?range=24h&points=288", got["saw_path"])
	}
}

// Path mode keeps working with Host routing enabled, as long as the request
// arrives on the control-plane host rather than a session subdomain.
func TestPathModeStillWorksAlongsideSubdomain(t *testing.T) {
	r := newSubdomainRelay()
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "host-a", Token: "amber-beam", TTL: time.Hour,
		ApprovalCode: "4827", Intent: "x", As: "@erik",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	// Control-plane host → path-routed landing.
	resp := reqWithHost(t, srv, "GET", "/h/amber-beam", "relay.fortytwowatts.com", "")
	body, _ := readBody(resp.Body)
	resp.Body.Close()
	html := string(body)
	if resp.StatusCode != 200 {
		t.Fatalf("path-mode landing status %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(html, `const APPROVE_PATH = "/h/amber-beam/approve";`) {
		t.Fatalf("path mode should use /h/<token>/approve:\n%s", html)
	}
	if !strings.Contains(html, `const DASHBOARD_PATH = "/h/amber-beam/web/";`) {
		t.Fatalf("path mode should use /h/<token>/web/:\n%s", html)
	}

	// healthz on the control-plane host still works.
	resp2 := reqWithHost(t, srv, "GET", "/healthz", "relay.fortytwowatts.com", "")
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("healthz status %d, want 200", resp2.StatusCode)
	}
}
