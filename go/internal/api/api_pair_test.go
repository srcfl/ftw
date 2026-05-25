package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPairStatusPostThenGet(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store, "/bin/true")

	body := `{"session_id":"abc","code":"7-x","intent":"goodwe","started_at":"2026-05-25T10:00:00Z","ttl_s":14400}`
	req := httptest.NewRequest("POST", "/api/pair/status", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("POST status: %d %s", w.Code, w.Body)
	}

	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, httptest.NewRequest("GET", "/api/pair/status", nil))
	if w2.Code != 200 {
		t.Fatalf("GET status: %d", w2.Code)
	}
	var got map[string]any
	json.Unmarshal(w2.Body.Bytes(), &got)
	if got["session_id"] != "abc" {
		t.Fatalf("expected echo: %v", got)
	}
}

func TestPairStatusGet404WhenNoSession(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store, "/bin/true")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/pair/status", nil))
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestPairAbortClearsStatus(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store, "/bin/true")
	store.Set(PairStatus{SessionID: "abc", Code: "7-x"})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/pair/abort", bytes.NewReader(nil)))
	if w.Code != 200 {
		t.Fatalf("abort: %d", w.Code)
	}
	if _, ok := store.Get(); ok {
		t.Fatal("status not cleared")
	}
}

// --- POST /api/pair/start tests ---

func TestPairStartReturns202(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	// Use /bin/true as the fake selfExe — exits immediately with 0, never
	// registers a status, but lets cmd.Run() succeed without blocking.
	RegisterPairRoutes(mux, store, "/bin/true")

	body := `{"intent":"write a goodwe driver","ttl":"2h"}`
	req := httptest.NewRequest("POST", "/api/pair/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if resp["status"] != "starting" {
		t.Fatalf("expected status=starting, got %v", resp)
	}
	if resp["ttl"] != "2h" {
		t.Fatalf("expected ttl=2h, got %v", resp)
	}
}

func TestPairStartRejectsBadTTL(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store, "/bin/true")

	body := `{"ttl":"notaduration"}`
	req := httptest.NewRequest("POST", "/api/pair/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body)
	}
}

func TestPairStartConflictWhenActive(t *testing.T) {
	store := NewPairStatusStore()
	store.Set(PairStatus{SessionID: "existing", Code: "3-foo"})
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store, "/bin/true")

	body := `{"ttl":"4h"}`
	req := httptest.NewRequest("POST", "/api/pair/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body)
	}
}
