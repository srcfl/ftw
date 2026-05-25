package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMCPServerBootAndShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := NewSession(ctx, SessionConfig{TTL: time.Hour})
	defer sess.End("test_cleanup")

	srv, err := StartMCP(ctx, MCPConfig{
		Addr:    "127.0.0.1:0",
		Session: sess,
		Audit:   NewAudit(),
		Tools:   nil, // empty registry
	})
	if err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	defer srv.Shutdown(context.Background())

	resp, err := http.Get("http://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "ok") {
		t.Fatalf("expected ok, got %d %s", resp.StatusCode, body)
	}
}
