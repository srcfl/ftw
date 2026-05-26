package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestMCPToolsListEmpty(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := NewSession(ctx, SessionConfig{TTL: time.Hour})
	defer sess.End("test_cleanup")

	srv, err := StartMCP(ctx, MCPConfig{
		Addr: "127.0.0.1:0", Session: sess, Audit: NewAudit(), Tools: nil,
	})
	if err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	defer srv.Shutdown(context.Background())

	rpc := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req, _ := http.NewRequest("POST", "http://"+srv.Addr()+"/mcp", bytes.NewReader([]byte(rpc)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	// Streamable HTTP responds either as plain JSON-RPC or SSE; parse out the JSON line.
	payload := string(body)
	if i := strings.Index(payload, "{"); i >= 0 {
		payload = payload[i:]
	}
	if j := strings.LastIndex(payload, "}"); j >= 0 {
		payload = payload[:j+1]
	}
	var rpcResp struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(payload), &rpcResp); err != nil {
		t.Fatalf("decode rpc body %q: %v", body, err)
	}
	if len(rpcResp.Result.Tools) != 0 {
		t.Fatalf("expected empty tools list, got %d entries", len(rpcResp.Result.Tools))
	}
}
