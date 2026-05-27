package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStartTunnelHostRegistersAndReturnsURL stands up a fake relay
// (just enough — POST /tunnel/register + a stubbed GET .../next that
// times out) and exercises the host's register-and-forward path.
func TestStartTunnelHostRegistersAndReturnsURL(t *testing.T) {
	relay := newFakeRelay(t)
	defer relay.Close()
	// relayAddrFlag is set from flag.String() in main(); tests must
	// provide a backing pointer themselves.
	oldFlag := relayAddrFlag
	url := relay.URL
	relayAddrFlag = &url
	defer func() { relayAddrFlag = oldFlag }()

	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("MCP-OK"))
	}))
	defer mcpSrv.Close()
	mcpAddr := strings.TrimPrefix(mcpSrv.URL, "http://")

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("DASH-OK"))
	}))
	defer apiSrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handle, err := StartTunnelHost(ctx, mcpAddr, apiSrv.URL, time.Minute, "test", "@bot")
	if err != nil {
		t.Fatalf("start tunnel host: %v", err)
	}
	if !strings.HasPrefix(handle.PublicURL, relay.URL+"/h/") {
		t.Fatalf("bad public URL: %s", handle.PublicURL)
	}
	if len(handle.ApprovalCode) != 4 {
		t.Fatalf("bad approval code: %q", handle.ApprovalCode)
	}
	if handle.Token == "" || !strings.Contains(handle.Token, "-") {
		t.Fatalf("token malformed: %q", handle.Token)
	}
	if handle.HostID == "" || !strings.HasPrefix(handle.HostID, "host-") {
		t.Fatalf("host-id malformed: %q", handle.HostID)
	}
}

func TestGenWordTokenHasSixWords(t *testing.T) {
	tok := genWordToken()
	parts := strings.Split(tok, "-")
	if len(parts) != 6 {
		t.Fatalf("expected 6 words, got %d: %q", len(parts), tok)
	}
	for i, p := range parts {
		if p == "" {
			t.Fatalf("word %d empty in %q", i, tok)
		}
	}
}

func TestGenApprovalCodeIsFourDigits(t *testing.T) {
	for i := 0; i < 100; i++ {
		c := genApprovalCode()
		if len(c) != 4 {
			t.Fatalf("attempt %d: code %q is not 4 chars", i, c)
		}
		for _, r := range c {
			if r < '0' || r > '9' {
				t.Fatalf("non-digit %q in code %q", r, c)
			}
		}
	}
}

func newFakeRelay(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tunnel/register", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"host_id"`) {
			http.Error(w, "missing host_id", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"public_url":"/h/x","approval_url":"/h/x/approve"}`))
	})
	mux.HandleFunc("GET /tunnel/{host_id}/next", func(w http.ResponseWriter, r *http.Request) {
		// Always time out so the host loop doesn't busy-spin during tests.
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}
