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

func TestSessionInfoTracksLandingHitsAndActivity(t *testing.T) {
	r := &Relay{
		Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry(),
		PollTimeout: 1 * time.Second,
	}
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "h", Token: "live", TTL: time.Hour, ApprovalCode: "0000",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	// Pre-state: pending, 0 hits, no activity.
	info := fetchSessionInfo(t, srv.URL, "live")
	if info.State != "pending" || info.PendingApprovals != 0 || info.LastActivityMs != 0 {
		t.Fatalf("pre-state: %+v", info)
	}

	// Friend hits landing page twice → pending_approvals = 2.
	_, _ = http.Get(srv.URL + "/h/live")
	_, _ = http.Get(srv.URL + "/h/live")
	info = fetchSessionInfo(t, srv.URL, "live")
	if info.PendingApprovals != 2 {
		t.Fatalf("after 2 landing hits: %+v", info)
	}

	// Approve. Then a tunneled request bumps activity.
	grant := approveGrant(t, srv, "live", "0000")

	// Spin up a host so /h/live/mcp completes the roundtrip.
	host := tunnel.NewHost(srv.URL, "h", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	host.PollTimeout = 500 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go host.Run(ctx)

	before := time.Now().UnixMilli()
	mreq, _ := http.NewRequest("POST", srv.URL+"/h/live/mcp", strings.NewReader(`{}`))
	mreq.Header.Set("Authorization", "Bearer "+grant)
	resp, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	info = fetchSessionInfo(t, srv.URL, "live")
	if info.State != "active" {
		t.Fatalf("post-approve state: %s", info.State)
	}
	if info.LastActivityMs < before {
		t.Fatalf("activity not bumped: %d < %d", info.LastActivityMs, before)
	}
}

func TestSessionInfoUnknownToken404(t *testing.T) {
	r := &Relay{Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry()}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/tunnel/sessions/nope/info")
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestSessionInfoExpiryReportedLazily verifies the State() side-effect
// of expiry is reflected via the info endpoint (the host's heartbeat
// would otherwise miss a TTL-expired session and keep showing it
// "active" on the dashboard).
func TestSessionInfoExpiryReportedLazily(t *testing.T) {
	r := &Relay{Queue: tunnel.NewQueue(), Tokens: NewTokenRegistry()}
	_, _ = r.Tokens.Register(TokenRegistration{
		HostID: "h", Token: "fade", TTL: 30 * time.Millisecond, ApprovalCode: "x",
	})
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	time.Sleep(80 * time.Millisecond)
	info := fetchSessionInfo(t, srv.URL, "fade")
	if info.State != "expired" {
		t.Fatalf("state should be expired, got %s", info.State)
	}
}

func fetchSessionInfo(t *testing.T, base, token string) SessionInfo {
	t.Helper()
	resp, err := http.Get(base + "/tunnel/sessions/" + token + "/info")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var si SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&si); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return si
}
