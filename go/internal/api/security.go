package api

import (
	"crypto/subtle"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/srcfl/ftw/go/internal/apiauth"
)

// MutationPolicy is the trust boundary for protected HTTP requests.
// Local LAN clients remain compatible without a token; public/FQDN access is
// opt-in and must prove possession of Token. Token also admits a LAN client
// when lan_auth is on — it is compared as the token, never hashed as the
// house password. LANAuthEnabled / VerifyLANSecret are an extra, optional
// house-password check on the LAN and do not replace Token.
type MutationPolicy struct {
	RequireTokenForRemote bool
	Token                 string
	// LANAuthEnabled reports whether the house opted into a LAN password.
	// Nil means off (today's behavior).
	LANAuthEnabled func() bool
	// VerifyLANSecret reports whether the presented secret matches the stored
	// house password. Nil or false means it does not.
	VerifyLANSecret func(secret string) bool
}

// WithSecurityHeaders sets clickjacking and baseline browser headers on
// every response, including 4xx/5xx, before the next handler runs.
func WithSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// Authenticate names the caller and guards state-changing requests.
//
// Naming the caller is the new half. This is the one place in the box that
// decides who is asking:
//
//   - a Caller already on the context was put there by whoever authenticated
//     the request — today the app session, which proved possession of an
//     enrolled device's Noise static key before a byte of this request
//     existed. It is kept as it is. KindApp is never replaced and never
//     asked for the house password.
//   - anything else arrived on the LAN listener. With api.lan_auth off
//     (the default), or from loopback, or with a matching house Bearer,
//     FTW_API_TOKEN, or session cookie, it is minted as a local owner —
//     today's behaviour. With lan_auth on, a LAN peer without that proof
//     is a viewer.
//
// The guarding half rejects browser cross-site writes, non-JSON request
// bodies, malformed Host/Origin metadata and unauthenticated protected
// requests addressed through non-local hostnames. Semantically active and
// secret-bearing GET/HEAD requests are protected too; ordinary reads remain
// unaffected. When lan_auth is on, protected LAN routes also need the
// house password, FTW_API_TOKEN, or the session cookie. Public-host
// checks stay independent and still use Token.
func Authenticate(next http.Handler, policy MutationPolicy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		houseOK, r := resolveLANSecret(r, policy)

		if _, ok := apiauth.From(r.Context()); !ok {
			r = r.WithContext(apiauth.WithCaller(r.Context(), decideLANCaller(r, policy, houseOK)))
		}

		if !requiresMutationProtection(r) {
			next.ServeHTTP(w, r)
			return
		}

		// Set before 401/403 so caches cannot store the denial either.
		w.Header().Set("Cache-Control", "no-store")

		reqAuthority, err := parseAuthority(r.Host)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid Host header"})
			return
		}
		if err := validateFetchMetadata(r, reqAuthority); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		remoteRequired := policy.RequireTokenForRemote && (!isLocalAuthority(reqAuthority) || !isLocalClient(r.RemoteAddr))
		if remoteRequired {
			if strings.TrimSpace(policy.Token) == "" {
				writeJSON(w, http.StatusForbidden, map[string]string{
					"error": "remote access to protected API routes is disabled; configure FTW_API_TOKEN or use a local address",
				})
				return
			}
			if !validBearerToken(r.Header.Get("Authorization"), policy.Token) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="ftw-api"`)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "valid Bearer token required"})
				return
			}
		}
		caller, _ := apiauth.From(r.Context())
		if !remoteRequired && lanAuthOn(policy) && !isLoopbackClient(r.RemoteAddr) &&
			caller.Kind == apiauth.KindLAN && !lanAuthExempt(r.URL.Path) && !houseOK {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ftw-lan"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "valid LAN password required"})
			return
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

func lanAuthOn(policy MutationPolicy) bool {
	return policy.LANAuthEnabled != nil && policy.LANAuthEnabled()
}

func lanAuthExempt(path string) bool {
	switch path {
	case "/api/auth/login", "/api/auth/logout", "/api/auth/status":
		return true
	default:
		return false
	}
}

func decideLANCaller(r *http.Request, policy MutationPolicy, houseOK bool) apiauth.Caller {
	if !lanAuthOn(policy) || isLoopbackClient(r.RemoteAddr) || houseOK {
		return localCaller(r)
	}
	return lanViewerCaller(r)
}

// resolveLANSecret verifies a presented API token, house Bearer or session
// cookie at most once per request. Bearer wins when both a Bearer and a
// cookie are present. FTW_API_TOKEN is compared as the token and is never
// counted as a house-password guess. The outer listener and Server.Handler
// both wrap Authenticate; a context flag stops the inner wrap from hashing
// or counting twice.
func resolveLANSecret(r *http.Request, policy MutationPolicy) (bool, *http.Request) {
	if ok, checked := lanSecretFrom(r.Context()); checked {
		return ok, r
	}
	if !lanAuthOn(policy) || isLoopbackClient(r.RemoteAddr) || !isLocalClient(r.RemoteAddr) {
		return false, r
	}
	if secret, ok := parseBearer(r.Header.Get("Authorization")); ok {
		houseOK := lanBearerSecretOK(policy, secret)
		return houseOK, r.WithContext(withLANSecret(r.Context(), houseOK))
	}
	if token, ok := lanSessionCookieValue(r); ok && lanSessionValid(token) {
		return true, r.WithContext(withLANSecret(r.Context(), true))
	}
	return false, r
}

// lanBearerSecretOK accepts FTW_API_TOKEN as the token, or the house
// password. A configured API token is compared first and never counted as
// a house-password guess, so a script retrying the token cannot lock
// Settings. The house password is still accepted as Bearer so the two
// secrets stay independent.
func lanBearerSecretOK(policy MutationPolicy, secret string) bool {
	want := policy.Token
	if want != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(want)) == 1 {
		return true
	}
	if strings.TrimSpace(want) != "" {
		return matchLANSecret(policy.VerifyLANSecret, secret)
	}
	return admitLANSecret(policy.VerifyLANSecret, secret)
}

// localCaller is what a request off the LAN listener carries.
//
// Full authority, because that is the truth of the deployment as it stands
// today: anyone who can reach the box's port can already do all of this with
// curl. The Subject records the address so an audit line can say where a
// change came from, which is more than the box could say before.
func localCaller(r *http.Request) apiauth.Caller {
	return apiauth.Caller{
		Subject: apiauth.KindLAN + ":" + remoteHost(r),
		Kind:    apiauth.KindLAN,
		Role:    apiauth.RoleOwner,
		Scopes:  apiauth.EveryScope(),
	}
}

func lanViewerCaller(r *http.Request) apiauth.Caller {
	return apiauth.Caller{
		Subject: apiauth.KindLAN + ":" + remoteHost(r),
		Kind:    apiauth.KindLAN,
		Role:    apiauth.RoleViewer,
		Scopes:  apiauth.NewScopeSet(apiauth.RoleScopes[apiauth.RoleViewer]...),
	}
}

func remoteHost(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return host
}

func requiresMutationProtection(r *http.Request) bool {
	switch r.Method {
	case http.MethodOptions:
		return false
	case http.MethodGet, http.MethodHead:
		if protectedReadPath(r.URL.Path) {
			return true
		}
		switch r.URL.Path {
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

// protectedReadPath names GET/HEAD routes that must not leak off a public
// host. Live dashboard reads are not listed here.
func protectedReadPath(path string) bool {
	switch path {
	case "/api/config",
		"/api/support/dump",
		"/api/support/report",
		"/api/assistant/status",
		"/api/logs",
		"/api/system/info",
		"/api/storage/inventory",
		"/api/research/load/dump",
		"/api/app-link/devices",
		"/api/devices",
		"/api/drivers/catalog",
		"/api/device_repository/status",
		"/api/components",
		"/api/components/history",
		"/api/ha/status",
		"/api/caldav/status",
		"/api/caldav/credentials",
		"/api/notifications/status",
		"/api/notifications/history",
		"/api/version/snapshots",
		"/api/scan",
		"/api/oauth/myuplink/start",
		"/api/drivers",
		"/api/ev/status",
		"/api/fleet-ping",
		"/api/notifications/rules",
		"/api/notifications/defaults",
		"/api/notifications/vapid",
		"/api/device_repository/catalog",
		"/api/app-link/status":
		return true
	}
	if path == "/api/backups" || strings.HasPrefix(path, "/api/backups/") {
		return true
	}
	// Ask why history holds the model's answers about this house, so the
	// list and every single conversation stay off a public host.
	if path == "/api/assistant/threads" || strings.HasPrefix(path, "/api/assistant/threads/") {
		return true
	}
	if path == "/api/series" || strings.HasPrefix(path, "/api/series/") {
		return true
	}
	if path == "/api/mpc/diagnose" || strings.HasPrefix(path, "/api/mpc/diagnose/") {
		return true
	}
	if strings.HasPrefix(path, "/api/device_repository/drivers/") && strings.HasSuffix(path, "/versions") {
		return true
	}
	if rest, ok := strings.CutPrefix(path, "/api/drivers/"); ok {
		// The detail route includes serial number, MAC address and endpoint.
		// Nested source, log and draft routes can hold credentials or arbitrary text.
		return (rest != "" && !strings.Contains(rest, "/")) ||
			strings.HasSuffix(rest, "/source") || strings.HasSuffix(rest, "/logs") ||
			strings.HasSuffix(rest, "/draft")
	}
	return false
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
	got, ok := parseBearer(header)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func parseBearer(header string) (string, bool) {
	parts := strings.Fields(header)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", false
	}
	return parts[1], true
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
	// A name with no dot is not local. `http://intranet` rebound onto
	// this box would otherwise skip the remote token. Keep `.local` —
	// that is the documented LAN name (ftw.local).
	return host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".home.arpa")
}

func isLocalClient(remoteAddr string) bool {
	ip := remoteIP(remoteAddr)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// isLoopbackClient is true only for 127.0.0.0/8 and ::1. A private LAN
// address is not loopback; with lan_auth on it still needs the house password.
func isLoopbackClient(remoteAddr string) bool {
	ip := remoteIP(remoteAddr)
	return ip != nil && ip.IsLoopback()
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	if zoneAt := strings.LastIndexByte(host, '%'); zoneAt >= 0 {
		host = host[:zoneAt]
	}
	return net.ParseIP(host)
}
