package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToolFtwAPI_GETStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/status" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"mode": "test"})
	}))
	defer upstream.Close()

	tool := NewFtwAPITool(upstream.URL)
	out, err := tool.Handle(context.Background(), map[string]any{
		"method": "GET",
		"path":   "/api/status",
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"mode":"test"`) {
		t.Fatalf("expected proxied body, got %s", b)
	}
}

func TestToolFtwAPI_RejectsAbsoluteURL(t *testing.T) {
	tool := NewFtwAPITool("http://localhost:8080")
	_, err := tool.Handle(context.Background(), map[string]any{
		"method": "GET",
		"path":   "http://attacker.example/api/x",
	})
	if err == nil {
		t.Fatal("expected reject of absolute URL")
	}
}
