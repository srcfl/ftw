package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPairStatusPostThenGet(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store, "/bin/true")

	now := time.Now().UTC().Format(time.RFC3339)
	body := fmt.Sprintf(`{"session_id":"abc","code":"7-x","intent":"goodwe","started_at":"%s","ttl_s":14400}`, now)
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

// TestPairStatusGet404WhenExpired verifies the self-heal behaviour: a stale
// PairStatus whose started_at + ttl_s lies in the past should be treated as
// gone and 404'd, with the store cleared as a side-effect. Without this, a
// sidecar that died without posting /api/pair/abort (kill -9, crash, container
// restart) would leave the dashboard stuck at "session active" forever and
// block POST /api/pair/start with a 409.
func TestPairStatusGet404WhenExpired(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store, "/bin/true")

	stale := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	store.Set(PairStatus{
		SessionID: "expired-abc",
		Code:      "7-x",
		StartedAt: stale,
		TTLS:      3600, // 1h TTL, started 2h ago → expired
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/pair/status", nil))
	if w.Code != 404 {
		t.Fatalf("expected 404 for expired session, got %d body %q", w.Code, w.Body)
	}
	// Side-effect: store was cleared so a follow-up POST /api/pair/start works.
	if _, ok := store.Get(); ok {
		t.Fatal("expected store to be cleared by GET after expiry")
	}
}

// TestPairStatusGetServesUnexpiredSession is the happy-path counterpart — make
// sure the expiry guard doesn't accidentally nuke live sessions.
func TestPairStatusGetServesUnexpiredSession(t *testing.T) {
	store := NewPairStatusStore()
	mux := http.NewServeMux()
	RegisterPairRoutes(mux, store, "/bin/true")

	fresh := time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339)
	store.Set(PairStatus{
		SessionID: "fresh-abc",
		Code:      "7-x",
		StartedAt: fresh,
		TTLS:      14400, // 4h TTL, started 30s ago → live
	})

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/pair/status", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200 for unexpired session, got %d body %q", w.Code, w.Body)
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
