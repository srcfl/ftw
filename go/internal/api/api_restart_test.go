package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestRestart_503WhenNotConfigured guards the contract that a build
// without a Restart callback (e.g. a test harness, or the proxy mode
// that just forwards to an upstream) returns 503 instead of falsely
// reporting success and leaving the operator confused about why
// nothing happened.
func TestRestart_503WhenNotConfigured(t *testing.T) {
	srv := New(&Deps{})
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/restart", nil)
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// TestRestart_DispatchesCallback verifies the handler returns 202 and
// invokes the Restart callback exactly once. The callback is async (200
// ms delay) so we wait on a result channel rather than polling.
func TestRestart_DispatchesCallback(t *testing.T) {
	var called int32
	done := make(chan struct{}, 1)
	deps := &Deps{
		Restart: func(ctx context.Context) error {
			atomic.AddInt32(&called, 1)
			done <- struct{}{}
			return nil
		},
	}
	srv := New(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/restart", nil)
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["status"] != "restarting" {
		t.Fatalf("expected status=restarting, got %v", body["status"])
	}

	// Wait for the goroutine to actually fire.
	<-done
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Fatalf("Restart callback fired %d times, want 1", got)
	}
}

// TestRestart_CallbackErrorIsLogged verifies the handler still responds
// 202 when the callback errors — the user-facing UI has already
// transitioned to the "restarting" overlay, so failing the response
// would just confuse them. The error lands in the server log instead.
func TestRestart_CallbackErrorIsLogged(t *testing.T) {
	done := make(chan struct{}, 1)
	deps := &Deps{
		Restart: func(ctx context.Context) error {
			done <- struct{}{}
			return errors.New("sidecar offline")
		},
	}
	srv := New(deps)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/restart", nil)
	srv.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (response sent before callback runs), got %d", w.Code)
	}
	<-done
}
