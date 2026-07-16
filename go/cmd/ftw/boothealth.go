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

// bootPhaseHandler answers health probes 200 while the process initializes,
// so a legitimately slow boot is distinguishable from a dead one. Everything
// else gets 503 + Retry-After so clients and the UI know to come back.
func bootPhaseHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"starting","phase":"initializing state"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"starting","phase":"initializing state"}`))
	})
	return mux
}
