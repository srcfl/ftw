package main

import (
	"net/http"
	"sync/atomic"
)

// swappableHandler lets the API port be bound before slow boot work (state
// integrity check, one-time VACUUM) and atomically hand the same listener
// over to the real mux once wiring completes. See main.go for why: an
// unbound port during a 25-minute compaction makes the Docker healthcheck
// fail and the self-update sidecar roll the deploy back mid-VACUUM.
type swappableHandler struct {
	// atomic.Pointer, not atomic.Value: the boot handler and the wired mux
	// are different concrete types, and Value panics on that.
	h atomic.Pointer[http.Handler]
}

func newSwappableHandler(initial http.Handler) *swappableHandler {
	s := &swappableHandler{}
	s.h.Store(&initial)
	return s
}

func (s *swappableHandler) Swap(h http.Handler) { s.h.Store(&h) }

func (s *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load()).ServeHTTP(w, r)
}

// bootPhaseHandler keeps the API port bound while state is opened or migrated,
// but deliberately fails both health and readiness. A control-plane updater
// must not commit a new Core image until the real mux is wired and /api/status
// is available; reporting 200 here would turn a long-running migration into a
// falsely successful update.
func bootPhaseHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"starting","ready":false,"phase":"initializing state"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"starting","phase":"initializing state"}`))
	})
	return mux
}
