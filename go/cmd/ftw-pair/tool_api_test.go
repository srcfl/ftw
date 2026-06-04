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

// ftw_api must enforce the /api/ allowlist and refuse owner-only control
// surfaces (pairing control, owner-access credential mgmt). Rejected paths error
// before any HTTP dial, so the unreachable base URL is never contacted.
func TestToolFtwAPI_DeniesOwnerOnlyAndNonAPI(t *testing.T) {
	tool := NewFtwAPITool("http://127.0.0.1:1")
	for _, path := range []string{
		"/healthz",                       // not under /api/
		"/api/pair/status",               // owner-only pairing control
		"/api/pair/start",                //
		"/api/owner-access/enroll/start", // owner-only credential management
		"/api/owner-access/devices",      //
	} {
		if _, err := tool.Handle(context.Background(), map[string]any{"method": "POST", "path": path}); err == nil {
			t.Errorf("path %q should be rejected by ftw_api, got nil error", path)
		}
	}
}

// denyOwnerOnly (the dashboard-proxy guard) must 403 owner-only paths and pass
// the intended friend energy API through.
func TestDenyOwnerOnly(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := denyOwnerOnly(next)
	cases := map[string]int{
		"/api/status":                      http.StatusOK,
		"/api/config":                      http.StatusOK, // intended friend energy-write
		"/api/pair/status":                 http.StatusForbidden,
		"/api/owner-access/whoami":         http.StatusForbidden,
		"/api/owner-access/devices/abc123": http.StatusForbidden,
	}
	for path, want := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, nil))
		if rec.Code != want {
			t.Errorf("denyOwnerOnly %q: got %d, want %d", path, rec.Code, want)
		}
	}
}
