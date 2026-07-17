// Package proxy implements an optional dev-mode reverse proxy for the
// HTTP handler. When enabled, requests under /api/ are forwarded to an
// upstream FTW instance so the local dev server can render
// the UI against live data without running real drivers locally.
//
// Static assets (/, /index.html, /components/*.css, *.js, …)
// always serve locally — those are the files being iterated on.
//
// Safety: read-only mode (on by default when an upstream is configured)
// blocks any non-GET request to /api/ with a 403, so clicking Save in
// the dev UI can't accidentally mutate the live instance's config,
// mode, or battery models. Explicitly pass readOnly=false if you need
// writes during a specific debugging session.
package proxy

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// Config captures how the proxy behaves. Upstream must be a valid URL
// (scheme + host) pointing at a running FTW instance.
type Config struct {
	Upstream *url.URL
	ReadOnly bool
}

// Wrap returns an http.Handler that forwards /api/ requests to cfg.Upstream
// and delegates everything else to `next` (the local static + API mux).
// When cfg.ReadOnly is true, any non-GET/HEAD/OPTIONS request under /api/
// is rejected with a 403 before the forward happens.
func Wrap(next http.Handler, cfg Config) http.Handler {
	rp := httputil.NewSingleHostReverseProxy(cfg.Upstream)
	origDirector := rp.Director
	rp.Director = func(r *http.Request) {
		origDirector(r)
		// Rewrite Host so upstream logs the right target and any virtual-
		// host-style routing works. NewSingleHostReverseProxy leaves Host
		// alone by default because some backends need the client Host, but
		// talking to another FTW we want the upstream to own it.
		r.Host = cfg.Upstream.Host
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("proxy upstream error", "path", r.URL.Path, "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"error":"proxy: upstream unreachable (%s)"}`, cfg.Upstream.Host))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if cfg.ReadOnly && !isReadMethod(r.Method) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"error":"proxy read-only: %s %s blocked (set FTW_PROXY_READONLY=0 to allow)"}`,
				r.Method, r.URL.Path,
			))
			return
		}
		rp.ServeHTTP(w, r)
	})
}

func isReadMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}
