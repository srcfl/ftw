package api

import (
	"crypto/subtle"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// MutationPolicy is the trust boundary for state-changing HTTP requests.
// Local LAN clients remain compatible without a token; public/FQDN access is
// opt-in and must prove possession of Token.
type MutationPolicy struct {
	RequireTokenForRemote bool
	Token                 string
}

// SecureMutations rejects browser cross-site writes, non-JSON request bodies,
// malformed Host/Origin metadata, and unauthenticated writes addressed through
// non-local hostnames. Semantically active GET/HEAD requests are protected too;
// ordinary read-only requests are intentionally unaffected.
func SecureMutations(next http.Handler, policy MutationPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requiresMutationProtection(r) {
			next.ServeHTTP(w, r)
			return
		}

		reqAuthority, err := parseAuthority(r.Host)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Host header"})
			return
		}
		if err := validateFetchMetadata(r, reqAuthority); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if policy.RequireTokenForRemote && (!isLocalAuthority(reqAuthority) || !isLocalClient(r.RemoteAddr)) {
			if strings.TrimSpace(policy.Token) == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "remote API mutations are disabled; configure FTW_API_TOKEN or use a local address",
				})
				return
			}
			if !validBearerToken(r.Header.Get("Authorization"), policy.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="ftw-api"`)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "valid Bearer token required"})
				return
			}
		}
		if requestHasBody(r) && !hasJSONContentType(r.Header.Get("Content-Type")) {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{
				"error": "request body must use Content-Type application/json",
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func requiresMutationProtection(r *http.Request) bool {
	switch r.Method {
	case http.MethodOptions:
		return false
	case http.MethodGet, http.MethodHead:
		switch r.URL.Path {
		case "/api/scan", "/api/oauth/myuplink/start":
			return true
		case "/api/version/check":
			return r.URL.Query().Get("force") == "1"
		case "/api/oauth/myuplink/callback":
			// The GET callback is an intentional cross-site redirect. Its
			// mutation is authorized by a short-lived, single-use state value
			// and PKCE in the handler. HEAD is never part of that flow.
			return r.Method == http.MethodHead
		default:
			return false
		}
	}
	return true
}

func requestHasBody(r *http.Request) bool {
	return r.Body != nil && r.Body != http.NoBody && (r.ContentLength != 0 || len(r.TransferEncoding) > 0)
}

func hasJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" ||
		(strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json"))
}

func validBearerToken(header, want string) bool {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(want)) == 1
}

type authority struct {
	host string
	port string
}

func parseAuthority(raw string) (authority, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.Contains(raw, ",") {
		return authority{}, http.ErrNotSupported
	}
	u, err := url.Parse("http://" + raw)
	if err != nil || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return authority{}, http.ErrNotSupported
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return authority{}, http.ErrNotSupported
	}
	return authority{host: host, port: u.Port()}, nil
}

func validateFetchMetadata(r *http.Request, req authority) error {
	fetchSiteValues := r.Header.Values("Sec-Fetch-Site")
	if len(fetchSiteValues) > 1 {
		return errCrossSiteMutation
	}
	if len(fetchSiteValues) == 1 {
		switch strings.ToLower(strings.TrimSpace(fetchSiteValues[0])) {
		case "", "same-origin", "none":
		case "same-site", "cross-site":
			return errCrossSiteMutation
		default:
			return errCrossSiteMutation
		}
	}

	origins := r.Header.Values("Origin")
	if len(origins) == 0 {
		return nil // Explicitly supported for curl, HA and other non-browser LAN clients.
	}
	if len(origins) != 1 || strings.Contains(origins[0], ",") {
		return errCrossSiteMutation
	}
	origin, err := url.Parse(origins[0])
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") ||
		origin.User != nil || origin.Host == "" || origin.Path != "" ||
		origin.RawQuery != "" || origin.Fragment != "" {
		return errCrossSiteMutation
	}
	originAuthority, err := parseAuthority(origin.Host)
	if err != nil || !sameAuthority(req, originAuthority, origin.Scheme) {
		return errCrossSiteMutation
	}
	return nil
}

var errCrossSiteMutation = &mutationSecurityError{"cross-site API mutation blocked"}

type mutationSecurityError struct{ message string }

func (e *mutationSecurityError) Error() string { return e.message }

func sameAuthority(req, origin authority, scheme string) bool {
	if req.host != origin.host {
		return false
	}
	if req.port == origin.port {
		return true
	}
	defaultPort := "80"
	if scheme == "https" {
		defaultPort = "443"
	}
	if req.port == "" {
		return origin.port == defaultPort
	}
	if origin.port == "" {
		return req.port == defaultPort
	}
	return false
}

func isLocalAuthority(a authority) bool {
	host := a.host
	if zoneAt := strings.LastIndexByte(host, '%'); zoneAt >= 0 {
		host = host[:zoneAt]
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".home.arpa") ||
		!strings.Contains(host, ".")
}

func isLocalClient(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if zoneAt := strings.LastIndexByte(host, '%'); zoneAt >= 0 {
		host = host[:zoneAt]
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}
