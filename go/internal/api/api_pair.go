package api

import (
	"encoding/json"
	"net/http"
	"sync"
)

// PairStatus is the metadata the ftw-pair sidecar POSTs to
// /api/pair/status so the dashboard can render the active session.
type PairStatus struct {
	SessionID string   `json:"session_id"`
	Code      string   `json:"code"`
	Intent    string   `json:"intent"`
	StartedAt string   `json:"started_at"`
	TTLS      int      `json:"ttl_s"`
	ToolCount int      `json:"tool_count,omitempty"`
	LastTools []string `json:"last_tools,omitempty"`
}

// PairStatusStore holds at most one active pair session in memory.
// Thread-safe. Nil cur means no active session.
type PairStatusStore struct {
	mu  sync.Mutex
	cur *PairStatus
}

// NewPairStatusStore allocates a ready-to-use store.
func NewPairStatusStore() *PairStatusStore { return &PairStatusStore{} }

// Set replaces the active session (overwrites any previous one).
func (s *PairStatusStore) Set(p PairStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = &p
}

// Get returns the current session and true, or zero value + false when
// no session is active.
func (s *PairStatusStore) Get() (PairStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return PairStatus{}, false
	}
	return *s.cur, true
}

// Clear removes the active session. The sidecar polls GET /api/pair/status;
// when it receives 404 it ends the session and exits.
func (s *PairStatusStore) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = nil
}

// RegisterPairRoutes wires /api/pair/* into mux.
//
//	GET  /api/pair/status  — returns the active session or 404
//	POST /api/pair/status  — sidecar registers/updates its session
//	POST /api/pair/abort   — operator clears the session; sidecar exits on next poll
func RegisterPairRoutes(mux *http.ServeMux, store *PairStatusStore) {
	mux.HandleFunc("GET /api/pair/status", func(w http.ResponseWriter, r *http.Request) {
		p, ok := store.Get()
		if !ok {
			http.Error(w, `{"error":"no active session"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("POST /api/pair/status", func(w http.ResponseWriter, r *http.Request) {
		var p PairStatus
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		store.Set(p)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/pair/abort", func(w http.ResponseWriter, r *http.Request) {
		// Clearing the store is the signal — the sidecar polls
		// GET /api/pair/status; when it returns 404 it calls
		// sess.End("aborted_by_owner") and exits cleanly.
		store.Clear()
		w.WriteHeader(http.StatusOK)
	})
}
