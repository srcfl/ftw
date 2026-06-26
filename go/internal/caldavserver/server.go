// Package caldavserver is a native, in-process CalDAV server (PROTOTYPE) built
// on github.com/emersion/go-webdav (MIT). It is an alternative to the bundled
// Radicale sidecar (GPLv3): being pure-Go and in-process, it ships in the
// single 42W binary and needs no second container — so the calendar feature
// (#498) can run in a single-container Home Assistant add-on too, and the GPL
// arm's-length constraint disappears.
//
// Selected with `caldav.server: native`. 42W's existing calendar client
// (internal/calendar) still talks CalDAV over localhost, so the inbound/outbound
// intent logic is unchanged — this just replaces "what the client connects to".
//
// PROTOTYPE limitations: in-memory storage (see backend.go) and no server-side
// recurrence expansion. Not yet a drop-in replacement for Radicale.
package caldavserver

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"time"

	"github.com/emersion/go-webdav/caldav"
)

// Server is the native CalDAV HTTP server: a go-webdav caldav.Handler behind
// HTTP Basic auth, on its own listener (default :5232, same port Radicale would
// use — you run one or the other).
type Server struct {
	addr    string
	httpSrv *http.Server
}

// New builds the server. principal is the CalDAV principal path (e.g.
// "/fortytwowatts/"); calendarPaths are collections to pre-create.
func New(addr, username, password, principal string, calendarPaths []string) *Server {
	mux := http.NewServeMux()
	mux.Handle("/", NewHandler(username, password, principal, calendarPaths))
	return &Server{
		addr:    addr,
		httpSrv: &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second},
	}
}

// NewHandler builds the auth-wrapped CalDAV http.Handler. Exposed so callers
// (and tests) can mount the native server on an existing mux / httptest server.
func NewHandler(username, password, principal string, calendarPaths []string) http.Handler {
	return basicAuth(username, password, &caldav.Handler{Backend: newMemBackend(principal, calendarPaths)})
}

// basicAuth gates the handler with a constant-time Basic-auth check. An empty
// configured password rejects everything (fail-closed) so a missing managed
// credential never opens the calendar.
func basicAuth(username, password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		authed := ok && password != "" &&
			subtle.ConstantTimeCompare([]byte(u), []byte(username)) == 1 &&
			subtle.ConstantTimeCompare([]byte(p), []byte(password)) == 1
		if !authed {
			w.Header().Set("WWW-Authenticate", `Basic realm="forty-two-watts"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Start begins serving in a background goroutine.
func (s *Server) Start() {
	go func() {
		slog.Info("caldav: native CalDAV server listening", "addr", s.addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("caldav: native server stopped", "err", err)
		}
	}()
}

// Stop shuts the server down.
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(ctx)
}
