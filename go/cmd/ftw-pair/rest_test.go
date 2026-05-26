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

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubTool is a minimal Tool implementation for REST-layer testing — it
// echoes back the args it was called with so we can assert dispatch + audit.
type stubTool struct {
	name    string
	handler func(ctx context.Context, args map[string]any) (any, error)
}

func (s *stubTool) Name() string { return s.name }
func (s *stubTool) Schema() *mcpsdk.Tool {
	return &mcpsdk.Tool{
		Name:        s.name,
		Description: "stub tool for testing",
		InputSchema: map[string]any{"type": "object"},
	}
}
func (s *stubTool) Handle(ctx context.Context, args map[string]any) (any, error) {
	return s.handler(ctx, args)
}

func startTestServer(t *testing.T, tools []Tool, audit *Audit) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess := NewSession(ctx, SessionConfig{TTL: time.Hour})
	t.Cleanup(func() { sess.End("test_cleanup") })

	srv, err := StartMCP(ctx, MCPConfig{
		Addr:    "127.0.0.1:0",
		Session: sess,
		Audit:   audit,
		Tools:   tools,
	})
	if err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv.Addr()
}

func TestRESTGetToolsCatalog(t *testing.T) {
	tools := []Tool{
		&stubTool{name: "echo", handler: func(ctx context.Context, args map[string]any) (any, error) { return args, nil }},
	}
	addr := startTestServer(t, tools, NewAudit())

	resp, err := http.Get("http://" + addr + "/tools")
	if err != nil {
		t.Fatalf("get /tools: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var catalog toolCatalog
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(catalog.Tools) != 1 || catalog.Tools[0].Name != "echo" {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if catalog.Tools[0].Description != "stub tool for testing" {
		t.Fatalf("missing description: %+v", catalog.Tools[0])
	}
	if catalog.Tools[0].InputSchema == nil {
		t.Fatalf("missing input_schema")
	}
}

func TestRESTPostToolDispatches(t *testing.T) {
	called := false
	tools := []Tool{
		&stubTool{
			name: "echo",
			handler: func(ctx context.Context, args map[string]any) (any, error) {
				called = true
				return map[string]any{"got": args}, nil
			},
		},
	}
	audit := NewAudit()
	addr := startTestServer(t, tools, audit)

	body := bytes.NewReader([]byte(`{"hello":"world"}`))
	resp, err := http.Post("http://"+addr+"/tools/echo", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}
	if !called {
		t.Fatalf("tool handler was not invoked")
	}
	if audit.ToolCount() != 1 {
		t.Fatalf("expected 1 audit entry, got %d", audit.ToolCount())
	}
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"hello":"world"`) {
		t.Fatalf("response missing echoed args: %s", raw)
	}
}

func TestRESTUnknownTool404(t *testing.T) {
	addr := startTestServer(t, nil, NewAudit())
	resp, err := http.Post("http://"+addr+"/tools/nope", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRESTMalformedBodyIs400(t *testing.T) {
	tools := []Tool{
		&stubTool{name: "echo", handler: func(ctx context.Context, args map[string]any) (any, error) { return nil, nil }},
	}
	addr := startTestServer(t, tools, NewAudit())
	resp, err := http.Post("http://"+addr+"/tools/echo", "application/json", strings.NewReader("not-json"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRESTToolErrorIs502(t *testing.T) {
	tools := []Tool{
		&stubTool{
			name: "fail",
			handler: func(ctx context.Context, args map[string]any) (any, error) {
				return nil, errStub("boom")
			},
		},
	}
	audit := NewAudit()
	addr := startTestServer(t, tools, audit)

	resp, err := http.Post("http://"+addr+"/tools/fail", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 502 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 502, got %d: %s", resp.StatusCode, raw)
	}
	// Audit still records the failure.
	if audit.ToolCount() != 1 {
		t.Fatalf("expected 1 audit entry, got %d", audit.ToolCount())
	}
	if audit.Events()[0].OutcomeOK {
		t.Fatalf("expected outcome_ok=false")
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }
