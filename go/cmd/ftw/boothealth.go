package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/srcfl/ftw/go/internal/state"
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

type bootHandler struct {
	webDir    string
	migration atomic.Pointer[json.RawMessage]
}

func bootPhaseHandler() http.Handler {
	return newBootPhaseHandler("")
}

func newBootPhaseHandler(webDir string) *bootHandler {
	return &bootHandler{webDir: webDir}
}

func (b *bootHandler) setMigration(progress state.HistoryMigrationStatus) {
	data, err := json.Marshal(progress)
	if err != nil {
		return
	}
	snapshot := json.RawMessage(data)
	b.migration.Store(&snapshot)
}

// Health proves process liveness. Every other API stays unavailable until
// the fully wired handler replaces this one. Browser reloads get a progress
// page instead of a JSON error, without changing the updater's readiness test.
func (b *bootHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if b.webDir != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if r.URL.Path == "/history-migration.js" {
			http.ServeFile(w, r, filepath.Join(b.webDir, "history-migration.js"))
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			if page, err := os.ReadFile(filepath.Join(b.webDir, "boot.html")); err == nil {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusServiceUnavailable)
				if r.Method != http.MethodHead {
					_, _ = w.Write(page)
				}
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	payload := map[string]any{"phase": "initializing state"}
	if migration := b.migration.Load(); migration != nil {
		payload["migration"] = *migration
	}
	if r.URL.Path == "/api/health" {
		payload["status"] = "starting"
	} else {
		payload["error"] = "starting"
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(payload)
}
