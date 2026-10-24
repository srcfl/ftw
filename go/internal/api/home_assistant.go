package api

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

// Supervisor authenticates the HA user before forwarding ingress requests.
// Never infer that trust from a forwarded header on the ordinary LAN listener.
type homeAssistantContextKey struct{}
type homeAssistantURLKey struct{}

func HomeAssistantIngress(r *http.Request) bool {
	trusted, _ := r.Context().Value(homeAssistantContextKey{}).(bool)
	return trusted
}

var ingressPath = regexp.MustCompile(`^/api/hassio_ingress/[A-Za-z0-9_-]+$`)

// WithHomeAssistantIngress belongs only on the HA bundle's ingress listener.
// It admits Supervisor, plus loopback GET /api/health for Docker's healthcheck.
// API responses and streaming requests pass through without body rewriting.
func WithHomeAssistantIngress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackClient(r.RemoteAddr) && r.Method == http.MethodGet && r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		prefix := r.Header.Get("X-Ingress-Path")
		if remoteHost(r) != "172.30.32.2" || !ingressPath.MatchString(prefix) || r.Header.Get("X-Remote-User-Id") == "" {
			http.Error(w, "Open FTW through Home Assistant", http.StatusForbidden)
			return
		}
		authority, err := parseAuthority(r.Host)
		if err != nil || (requiresMutationProtection(r) && validateFetchMetadata(r, authority) != nil) {
			http.Error(w, "invalid Home Assistant request origin", http.StatusForbidden)
			return
		}
		externalBase := requestOrigin(r) + prefix
		ctx := context.WithValue(r.Context(), homeAssistantURLKey{}, externalBase)
		r = r.Clone(context.WithValue(ctx, homeAssistantContextKey{}, true))
		// This is an authenticated local owner, including when an existing install
		// still has a house password. Preserve the Supervisor peer for local gates.
		r = r.WithContext(withLANSecret(r.Context(), true))
		r = r.WithContext(apiauth.WithCaller(r.Context(), apiauth.Caller{
			Subject: "ha:" + r.Header.Get("X-Remote-User-Id"), Kind: apiauth.KindLAN,
			Role: apiauth.RoleOwner, Scopes: apiauth.EveryScope(),
		}))
		// Check HA's origin above, then present the authenticated local request to
		// existing API guards. Ordinary proxies never reach this normalization.
		r.Host = "ftw.local"
		if r.Header.Get("Origin") != "" {
			r.Header.Set("Origin", "http://ftw.local")
		}
		for name := range r.Header {
			lower := strings.ToLower(name)
			if lower == "forwarded" || lower == "x-real-ip" || strings.HasPrefix(lower, "x-forwarded-") {
				r.Header.Del(name)
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		// The on-box UI uses root URLs. Adapt only its static HTML/JS/CSS to HA's
		// installation-specific prefix; never rewrite user data, API JSON or files.
		r.Header.Del("Range")
		r.Header.Del("If-Modified-Since")
		r.Header.Del("If-None-Match")
		response := &ingressResponse{headers: make(http.Header)}
		next.ServeHTTP(response, r)
		for name, values := range response.Header() {
			w.Header()[name] = values
		}
		if location := w.Header().Get("Location"); strings.HasPrefix(location, "/") && !strings.HasPrefix(location, "//") {
			w.Header().Set("Location", prefix+location)
		}
		body := response.body.Bytes()
		kind := w.Header().Get("Content-Type")
		if strings.HasPrefix(kind, "text/html") || strings.Contains(kind, "javascript") || strings.HasPrefix(kind, "text/css") {
			body = ingressAsset(body, prefix, strings.HasPrefix(kind, "text/html"))
			w.Header().Del("Content-Length")
			w.Header().Del("Etag")
			w.Header().Set("Cache-Control", "no-store")
		}
		w.WriteHeader(response.statusCode())
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}

// Match complete root URL starts, not arbitrary slashes (split('/'), API path
// suffixes such as '/draft', protocol-relative URLs, or data in API replies).
var ingressAssetURL = regexp.MustCompile("([\"'`(])(/(?:api/|components/|settings/|vendor/|setup(?:[?\"'`#]|$)|[A-Za-z0-9_-]+\\.(?:js|mjs|css|html|svg|png|ico|woff2?)))")

func ingressAsset(body []byte, prefix string, html bool) []byte {
	body = ingressAssetURL.ReplaceAll(body, []byte("${1}"+prefix+"${2}"))
	for _, quote := range []string{"'", "\""} {
		for _, start := range []string{"window.location.href = ", "href="} {
			body = bytes.ReplaceAll(body, []byte(start+quote+"/"+quote), []byte(start+quote+prefix+"/"+quote))
		}
		body = bytes.ReplaceAll(body, []byte(quote+"/?"), []byte(quote+prefix+"/?"))
	}
	if html {
		script := `<script src="` + prefix + `/home-assistant.js" defer></script>`
		body = bytes.Replace(body, []byte("</head>"), []byte(script+"</head>"), 1)
	}
	return body
}

// Only static responses use this buffer. API handlers keep the original
// ResponseWriter, including its streaming and WebSocket interfaces.
type ingressResponse struct {
	headers http.Header
	body    bytes.Buffer
	status  int
}

func (w *ingressResponse) Header() http.Header { return w.headers }
func (w *ingressResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *ingressResponse) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.headers.Get("Content-Type") == "" {
		w.headers.Set("Content-Type", http.DetectContentType(body))
	}
	return w.body.Write(body)
}
func (w *ingressResponse) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
