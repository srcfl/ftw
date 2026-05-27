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

	// 2. Friend hits /h/tok1/mcp before approval → 425.
	pre, err := http.Post(srv.URL+"/h/tok1/mcp", "application/json", strings.NewReader(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if pre.StatusCode != http.StatusTooEarly {
		t.Fatalf("expected 425 before approval, got %d", pre.StatusCode)
	}

	// 3. Approve.
	apv, err := http.Post(srv.URL+"/h/tok1/approve", "application/json", strings.NewReader(`{"code":"4827"}`))
	if err != nil {
		t.Fatal(err)
	}
	if apv.StatusCode != http.StatusNoContent {
		t.Fatalf("approve status %d", apv.StatusCode)
	}

	// 4. Friend POSTs MCP request.
	post, err := http.Post(srv.URL+"/h/tok1/mcp", "application/json", strings.NewReader(`{"method":"ping"}`))
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
	_, _ = http.Post(srv.URL+"/h/tok2/approve", "application/json", strings.NewReader(`{"code":"1234"}`))

	r1, err := http.Get(srv.URL + "/h/tok2/web/")
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Body.Close()
	b1, _ := io.ReadAll(r1.Body)
	if string(b1) != "home" {
		t.Fatalf("/ → %q", b1)
	}
	if r1.Header.Get("X-Dashboard") != "yes" {
		t.Fatalf("header lost: %v", r1.Header)
	}

	r2, err := http.Get(srv.URL + "/h/tok2/web/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	b2, _ := io.ReadAll(r2.Body)
	if !strings.Contains(string(b2), `"ok":true`) {
		t.Fatalf("/api/status → %q", b2)
	}
}
