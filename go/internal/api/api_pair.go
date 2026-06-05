package api

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// pipeToLog scans the given reader line-by-line and emits each line at the
// given level with the given source attr. Used to surface the ftw-pair
// sidecar's stdout + stderr into the main service's log ring so silent
// failures are visible via GET /api/logs.
func pipeToLog(r io.Reader, source string, level slog.Level) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 1024*1024)
	for s.Scan() {
		slog.Log(nil, level, s.Text(), "source", source) //nolint:contextcheck
	}
}

// resolvedSelfExe returns os.Executable() with symlinks resolved, or
// os.Args[0] on error. Used as the default selfExe for spawning child
// pair sessions; tests inject a fake path via Deps.PairSelfExe.
func resolvedSelfExe() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	if len(os.Args) > 0 {
		return os.Args[0]
	}
	return ""
}

// PairStatus is the metadata the ftw-pair sidecar POSTs to
// /api/pair/status so the dashboard can render the active session.
type PairStatus struct {
	SessionID        string   `json:"session_id"`
	Code             string   `json:"code"`
	Intent           string   `json:"intent"`
	StartedAt        string   `json:"started_at"`
	TTLS             int      `json:"ttl_s"`
	ToolCount        int      `json:"tool_count,omitempty"`
	LastTools        []string `json:"last_tools,omitempty"`
	ClientsConnected int      `json:"clients_connected"`

	// PairURL is the full public URL the friend opens to join the
	// session. Empty when running with -no-relay (LAN-only). The
	// dashboard renders this verbatim — operator copies + sends.
	PairURL string `json:"pair_url,omitempty"`

	// ApprovalCode is the 4-digit voice-channel cross-check code.
	// Friend reads it from the relay landing page; operator types it
	// into the dashboard's Allow form. The matching POST flips the
	// relay's token state from pending → active.
	ApprovalCode string `json:"approval_code,omitempty"`

	// SessionState mirrors the relay-side token state ("pending",
	// "active", "expired", "revoked"). Updated by the sidecar's
	// heartbeat after polling /sessions/<token>/info on the relay.
	// Empty in LAN-only mode (no relay state to track).
	SessionState string `json:"session_state,omitempty"`

	// LastActivityMs is the millisecond Unix timestamp of the most
	// recent tunneled request from the friend (via the relay's
	// /sessions/<token>/info endpoint). 0 means no activity yet OR
	// LAN-only mode.
	LastActivityMs int64 `json:"last_activity_ms,omitempty"`

	// PendingApprovalsCount is how many landing-page hits the relay
	// has seen for a still-pending token. Surfaces to the dashboard
	// as "friend opened the URL — call you with the code".
	PendingApprovalsCount int `json:"pending_approvals_count,omitempty"`
}

// isExpired returns true when StartedAt + TTLS is in the past.
// Garbage strings are considered NOT expired (we don't want a parse glitch
// to nuke a live session) — the sidecar's own TTL check is the source of truth.
func (p PairStatus) isExpired(now time.Time) bool {
	if p.StartedAt == "" || p.TTLS <= 0 {
		return false
	}
	started, err := time.Parse(time.RFC3339, p.StartedAt)
	if err != nil {
		return false
	}
	expiry := started.Add(time.Duration(p.TTLS) * time.Second)
	return now.After(expiry)
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
//	POST /api/pair/start   — owner starts a new pair session from the web UI
//
// selfExe is the path to the running binary (os.Executable() result), used
// to spawn "self pair --ttl <t> [--intent <i>]" as a detached child.
// manage authorizes owner-only pairing control (start/abort). It is the strict
// owner authorizer (a real session or genuine LAN presence, never the loopback
// LAN-bypass), so a friend pair-flow request — which reverse-proxies from
// loopback — can't spawn new pair sidecars or abort the session. POST
// /api/pair/status is deliberately NOT gated this way: it is the sidecar's own
// loopback status channel.
func RegisterPairRoutes(mux *http.ServeMux, store *PairStatusStore, selfExe string, manage func(*http.Request) ([]byte, bool)) {
	mux.HandleFunc("GET /api/pair/status", func(w http.ResponseWriter, r *http.Request) {
		p, ok := store.Get()
		if !ok {
			http.Error(w, `{"error":"no active session"}`, http.StatusNotFound)
			return
		}
		// Self-heal stale records: if the session's TTL has already elapsed
		// the sidecar should have torn down by now. Treat as gone — flip the
		// dashboard to the start form and unblock POST /api/pair/start.
		// Without this, a sidecar that exited without posting /api/pair/abort
		// (kill -9, container restart) leaves the card stuck at "active".
		if p.isExpired(time.Now()) {
			store.Clear()
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
		if _, ok := manage(r); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Clearing the store is the signal — the sidecar polls
		// GET /api/pair/status; when it returns 404 it calls
		// sess.End("aborted_by_owner") and exits cleanly.
		store.Clear()
		w.WriteHeader(http.StatusOK)
	})
	start := handlePairStart(store, selfExe)
	mux.HandleFunc("POST /api/pair/start", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := manage(r); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		start(w, r)
	})
}

// handlePairStart returns an http.HandlerFunc that spawns
// "<selfExe> pair --ttl <ttl> [--intent <intent>]" as a detached child
// process. The child will register itself via POST /api/pair/status once
// the subetha relay tunnel is up; the card's fast-poll loop picks that up.
func handlePairStart(store *PairStatusStore, selfExe string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Intent string `json:"intent"`
			TTL    string `json:"ttl"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.TTL == "" {
			body.TTL = "4h"
		}
		if _, err := time.ParseDuration(body.TTL); err != nil {
			http.Error(w, fmt.Sprintf("invalid ttl: %v", err), http.StatusBadRequest)
			return
		}
		if _, ok := store.Get(); ok {
			w.Header().Set("Content-Type", "application/json")
			http.Error(w, `{"error":"pair session already active"}`, http.StatusConflict)
			return
		}
		args := []string{"pair", "--ttl", body.TTL}
		if body.Intent != "" {
			args = append(args, "--intent", body.Intent)
		}
		go func() {
			cmd := exec.Command(selfExe, args...)
			// Pipe child stdout + stderr into our slog. Previously these were
			// discarded, which made silent fowld failures (e.g. unsupported
			// command on a different fowl version) effectively invisible:
			// /api/pair/status would stay at 404 forever and the operator
			// had no way to see why. Now each child line lands in the log
			// ring and surfaces via GET /api/logs.
			stdout, _ := cmd.StdoutPipe()
			stderr, _ := cmd.StderrPipe()
			if err := cmd.Start(); err != nil {
				slog.Error("pair: spawn sidecar", "err", err)
				return
			}
			go pipeToLog(stdout, "pair.stdout", slog.LevelInfo)
			go pipeToLog(stderr, "pair.stderr", slog.LevelWarn)
			if err := cmd.Wait(); err != nil {
				slog.Warn("pair: sidecar exited", "err", err)
			}
		}()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]any{"status": "starting", "ttl": body.TTL})
	}
}
