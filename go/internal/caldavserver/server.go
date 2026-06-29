// Package caldavserver is 42W's native, in-process CalDAV server built on
// github.com/emersion/go-webdav (MIT). Being pure-Go and in-process, it ships
// in the single 42W binary and needs no second container — so the calendar
// feature (#498) runs everywhere 42W does, including a single-container Home
// Assistant add-on.
//
// 42W's calendar client (internal/calendar) talks CalDAV to it over localhost,
// so the inbound/outbound intent logic is independent of transport.
//
// Objects persist via a Store (state.db in production; in-memory for tests).
// Recurring events ARE expanded server-side per RFC 4791 CALDAV:expand (see
// expand.go). Known limits: a single principal and minimal MKCALENDAR/sync
// semantics; interop is verified against 42W's own go-webdav client rather than
// the full matrix of iOS / Google / Thunderbird.
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
// HTTP Basic auth, on its own listener (default :5232).
type Server struct {
	addr    string
	httpSrv *http.Server
}

// Option configures optional server behaviour without breaking the core
// New / NewHandler signatures.
type Option func(*options)

type options struct {
	feeds map[string]string // feed name -> collection path (read-only .ics feeds)
}

// WithFeeds exposes read-only aggregated .ics feeds at /feed/<name>.ics for the
// given collections (e.g. {"plan": "/u/plan/", "history": "/u/history/"}), so a
// phone can subscribe via a one-tap webcal:// link. Served behind the same
// Basic auth as the rest of the server.
func WithFeeds(feeds map[string]string) Option {
	return func(o *options) { o.feeds = feeds }
}

func buildOptions(opts []Option) options {
	var o options
	for _, fn := range opts {
		fn(&o)
	}
	return o
}

// New builds the server. principal is the CalDAV principal path (e.g.
// "/fortytwowatts/"); calendarPaths are collections to pre-create; store is the
// persistence (pass *state.Store for durability, or nil for in-memory).
func New(addr, username, password, principal string, calendarPaths []string, store Store, opts ...Option) *Server {
	return &Server{
		addr:    addr,
		httpSrv: &http.Server{Addr: addr, Handler: NewHandler(username, password, principal, calendarPaths, store, opts...), ReadHeaderTimeout: 10 * time.Second},
	}
}

// NewHandler builds the auth-wrapped CalDAV http.Handler. Exposed so callers
// (and tests) can mount the native server on an existing mux / httptest server.
func NewHandler(username, password, principal string, calendarPaths []string, store Store, opts ...Option) http.Handler {
	o := buildOptions(opts)
	mux := http.NewServeMux()
	if len(o.feeds) > 0 {
		mux.Handle("/feed/", basicAuth(username, password, newFeedHandler(o.feeds, store)))
	}
	mux.Handle("/", basicAuth(username, password, &caldav.Handler{Backend: newBackend(principal, calendarPaths, store)}))
	return mux
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
