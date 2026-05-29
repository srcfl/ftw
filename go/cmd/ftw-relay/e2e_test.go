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

// TestE2EHostAndFriendRoundtripThroughRelay is the canary: register a
// token, approve it, fire an MCP-style POST from the friend side, get
// the host's response back. All in-process, no network beyond loopback.
func TestE2EHostAndFriendRoundtripThroughRelay(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		PollTimeout: 1 * time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	// Local MCP-like backend the host will forward to.
	mcpBackend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"echo":` + string(body) + `}`))
	})

	host := tunnel.NewHost(srv.URL, "host-a", mcpBackend)
	host.PollTimeout = 1 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

	// 1. Register.
	regBody, _ := json.Marshal(registerRequest{
		HostID: "host-a", Token: "tok1", TTLMs: 60_000, ApprovalCode: "4827",
	})
	regResp, err := http.Post(srv.URL+"/tunnel/register", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatal(err)
	}
	if regResp.StatusCode != 200 {
		t.Fatalf("register status %d", regResp.StatusCode)
	}

	// 2. Friend hits /h/tok1/mcp before approval → 401 (no session grant yet).
	pre, err := http.Post(srv.URL+"/h/tok1/mcp", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if pre.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 before approval, got %d", pre.StatusCode)
	}

	// 3. Approve → mints + returns the session grant.
	grant := approveGrant(t, srv, "tok1", "4827")

	// 3b. /mcp WITHOUT the grant is still rejected after activation
	//     (a leaked-but-active URL is useless without the secret).
	noGrant, err := http.Post(srv.URL+"/h/tok1/mcp", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if noGrant.StatusCode != http.StatusUnauthorized {
		t.Fatalf("active /mcp without grant: got %d, want 401", noGrant.StatusCode)
	}

	// 4. Friend POSTs MCP request WITH the Bearer grant.
	mreq, _ := http.NewRequest("POST", srv.URL+"/h/tok1/mcp", strings.NewReader(`{"method":"ping"}`))
	mreq.Header.Set("Content-Type", "application/json")
	mreq.Header.Set("Authorization", "Bearer "+grant)
	post, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatal(err)
	}
	defer post.Body.Close()
	if post.StatusCode != 200 {
		body, _ := io.ReadAll(post.Body)
		t.Fatalf("mcp status %d: %s", post.StatusCode, body)
	}
	body, _ := io.ReadAll(post.Body)
	if !strings.Contains(string(body), `"echo":{"method":"ping"}`) {
		t.Fatalf("response did not contain echo: %q", body)
	}
}

func TestE2EWebReverseProxy(t *testing.T) {
	relay := &Relay{
		Queue:       tunnel.NewQueue(),
		Tokens:      NewTokenRegistry(),
		PollTimeout: 1 * time.Second,
	}
	srv := httptest.NewServer(relay.Handler())
	defer srv.Close()

	// Local "dashboard" backend.
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Dashboard", "yes")
		switch r.URL.Path {
		case "/":
			_, _ = w.Write([]byte("home"))
		case "/api/status":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r)
		}
	})

	host := tunnel.NewHost(srv.URL, "host-b", dashboard)
	host.PollTimeout = 1 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

	regBody, _ := json.Marshal(registerRequest{HostID: "host-b", Token: "tok2", TTLMs: 60_000, ApprovalCode: "1234"})
	_, _ = http.Post(srv.URL+"/tunnel/register", "application/json", bytes.NewReader(regBody))
	grant := approveGrant(t, srv, "tok2", "1234")

	// Web access requires the grant cookie; without it → 401.
	noCookie, err := http.Get(srv.URL + "/h/tok2/web/")
	if err != nil {
		t.Fatal(err)
	}
	noCookie.Body.Close()
	if noCookie.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/web without grant cookie: got %d, want 401", noCookie.StatusCode)
	}

	r1 := getWithGrantCookie(t, srv.URL+"/h/tok2/web/", grant)
	defer r1.Body.Close()
	b1, _ := io.ReadAll(r1.Body)
	if string(b1) != "home" {
		t.Fatalf("/ → %q", b1)
	}
	if r1.Header.Get("X-Dashboard") != "yes" {
		t.Fatalf("header lost: %v", r1.Header)
	}

	r2 := getWithGrantCookie(t, srv.URL+"/h/tok2/web/api/status", grant)
	defer r2.Body.Close()
	b2, _ := io.ReadAll(r2.Body)
	if !strings.Contains(string(b2), `"ok":true`) {
		t.Fatalf("/api/status → %q", b2)
	}
}

// approveGrant POSTs the 4-digit code and returns the session grant minted
// by the relay. Fails the test on any non-200 or missing grant.
func approveGrant(t *testing.T, srv *httptest.Server, token, code string) string {
	t.Helper()
	resp, err := http.Post(srv.URL+"/h/"+token+"/approve", "application/json",
		strings.NewReader(`{"code":"`+code+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve status %d, want 200", resp.StatusCode)
	}
	var out struct {
		Grant string `json:"grant"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode approve body: %v", err)
	}
	if out.Grant == "" {
		t.Fatal("approve returned empty grant")
	}
	return out.Grant
}

// getWithGrantCookie issues a GET carrying the grant cookie (the Secure
// flag means httptest's http client won't send it from a jar, so we set
// the header directly).
func getWithGrantCookie(t *testing.T, url, grant string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Cookie", grantCookie+"="+grant)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
