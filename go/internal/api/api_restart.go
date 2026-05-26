package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// handleRestart triggers a graceful process restart so the binary picks
// up changes that the configreload watcher can't apply in flight (the
// dwindling list in config.RestartRequiredFor).
//
// The handler always returns first and triggers the restart from a
// background goroutine: the caller (typically the Settings dialog) needs
// to render its "Restarting…" overlay before the HTTP server starts
// shutting down, otherwise the response gets dropped mid-flight and the
// UI shows a generic network error.
//
// The 200 ms delay between sending the response and signaling the
// restart is the smallest window we observed reliably gives the kernel
// time to flush the TCP buffer back to the browser. Shorter and the
// dialog never sees a 202.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.Restart == nil {
		writeJSON(w, 503, map[string]string{"error": "restart not configured"})
		return
	}
	writeJSON(w, 202, map[string]any{"status": "restarting"})

	go func() {
		time.Sleep(200 * time.Millisecond)
		// Don't reuse r.Context() — it's canceled the instant the
		// response is flushed, and the sidecar dial path needs a live
		// context to negotiate the unix socket.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.deps.Restart(ctx); err != nil {
			slog.Warn("restart trigger failed", "err", err)
		}
	}()
}
