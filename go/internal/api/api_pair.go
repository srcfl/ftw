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
//	POST /api/pair/start   — owner starts a new pair session from the web UI
//
// selfExe is the path to the running binary (os.Executable() result), used
// to spawn "self pair --ttl <t> [--intent <i>]" as a detached child.
func RegisterPairRoutes(mux *http.ServeMux, store *PairStatusStore, selfExe string) {
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
	mux.HandleFunc("POST /api/pair/start", handlePairStart(store, selfExe))
}

// handlePairStart returns an http.HandlerFunc that spawns
// "<selfExe> pair --ttl <ttl> [--intent <intent>]" as a detached child
// process. The child will register itself via POST /api/pair/status once
// the wormhole tunnel is up; the card's fast-poll loop picks that up.
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
