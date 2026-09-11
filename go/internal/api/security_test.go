package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testMutationToken = "0123456789abcdef0123456789abcdef"

type sensitiveMutation struct {
	name string
	path string
	body string
}

var sensitiveMutations = []sensitiveMutation{
	{name: "battery", path: "/api/battery/manual_hold", body: `{"direction":"idle","hold_s":60}`},
	{name: "EV", path: "/api/ev/command", body: `{"action":"ev_stop"}`},
	{name: "update", path: "/api/version/update"},
	{name: "restore", path: "/api/version/rollback", body: `{"snapshot_id":"snapshot-1"}`},
	{name: "restart", path: "/api/restart"},
}

func TestAuthenticateBlocksBrowserCrossSiteSensitiveRoutes(t *testing.T) {
	for _, endpoint := range sensitiveMutations {
		t.Run(endpoint.name, func(t *testing.T) {
			called := false
			h := Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}), MutationPolicy{})

			req := mutationRequest(endpoint, "http://ftw.local:8080")
			req.Header.Set("Origin", "https://attacker.example")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
			}
			if called {
				t.Fatal("protected handler was called")
			}
			if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Fatalf("cross-origin response advertised CORS: %q", got)
			}
		})
	}
}

func TestAuthenticateTreatsEveryUnsafeHTTPMethodAsMutation(t *testing.T) {
	for _, method := range []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodConnect,
		http.MethodTrace,
	} {
		t.Run(method, func(t *testing.T) {
			called := false
			req := httptest.NewRequest(method, "http://ftw.local:8080/api/any", nil)
			req.RemoteAddr = "192.168.1.10:43210"
			req.Header.Set("Origin", "https://attacker.example")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rr := httptest.NewRecorder()
			Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			}), MutationPolicy{}).ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
			}
			if called {
				t.Fatal("protected handler was called")
			}
		})
	}
}

func TestAuthenticateGuardsProtectedReads(t *testing.T) {
	guarded := []struct {
		name   string
		method string
		path   string
	}{
		{name: "network scan", method: http.MethodGet, path: "/api/scan"},
		{name: "network scan via HEAD", method: http.MethodHead, path: "/api/scan"},
		{name: "forced update check", method: http.MethodGet, path: "/api/version/check?force=1"},
		{name: "forced update check via HEAD", method: http.MethodHead, path: "/api/version/check?force=1"},
		{name: "OAuth start", method: http.MethodGet, path: "/api/oauth/myuplink/start"},
		{name: "OAuth start via HEAD", method: http.MethodHead, path: "/api/oauth/myuplink/start"},
		{name: "OAuth callback via HEAD", method: http.MethodHead, path: "/api/oauth/myuplink/callback"},
		{name: "backup list", method: http.MethodGet, path: "/api/backups"},
		{name: "backup list via HEAD", method: http.MethodHead, path: "/api/backups"},
		{name: "backup download", method: http.MethodGet, path: "/api/backups/ftw-full-backup-20260731T120000Z.ftwbak"},
		{name: "backup download via HEAD", method: http.MethodHead, path: "/api/backups/ftw-full-backup-20260731T120000Z.ftwbak"},
		{name: "CalDAV credentials", method: http.MethodGet, path: "/api/caldav/credentials"},
		{name: "CalDAV credentials via HEAD", method: http.MethodHead, path: "/api/caldav/credentials"},
		{name: "config", method: http.MethodGet, path: "/api/config"},
		{name: "config via HEAD", method: http.MethodHead, path: "/api/config"},
		{name: "support dump", method: http.MethodGet, path: "/api/support/dump"},
		{name: "support dump via HEAD", method: http.MethodHead, path: "/api/support/dump"},
		{name: "support report", method: http.MethodGet, path: "/api/support/report"},
		{name: "support report via HEAD", method: http.MethodHead, path: "/api/support/report"},
		{name: "assistant status", method: http.MethodGet, path: "/api/assistant/status"},
		{name: "assistant status via HEAD", method: http.MethodHead, path: "/api/assistant/status"},
		{name: "Ask why history", method: http.MethodGet, path: "/api/assistant/threads"},
		{name: "Ask why history via HEAD", method: http.MethodHead, path: "/api/assistant/threads"},
		{name: "one Ask why conversation", method: http.MethodGet, path: "/api/assistant/threads/aaaa000000000001"},
		{name: "one Ask why conversation via HEAD", method: http.MethodHead, path: "/api/assistant/threads/aaaa000000000001"},
		{name: "logs", method: http.MethodGet, path: "/api/logs"},
		{name: "logs via HEAD", method: http.MethodHead, path: "/api/logs"},
		{name: "system info", method: http.MethodGet, path: "/api/system/info"},
		{name: "system info via HEAD", method: http.MethodHead, path: "/api/system/info"},
		{name: "storage inventory", method: http.MethodGet, path: "/api/storage/inventory"},
		{name: "storage inventory via HEAD", method: http.MethodHead, path: "/api/storage/inventory"},
		{name: "research dump", method: http.MethodGet, path: "/api/research/load/dump"},
		{name: "research dump via HEAD", method: http.MethodHead, path: "/api/research/load/dump"},
		{name: "app-link devices", method: http.MethodGet, path: "/api/app-link/devices"},
		{name: "app-link devices via HEAD", method: http.MethodHead, path: "/api/app-link/devices"},
		{name: "device identities", method: http.MethodGet, path: "/api/devices"},
		{name: "device identities via HEAD", method: http.MethodHead, path: "/api/devices"},
		{name: "driver catalog", method: http.MethodGet, path: "/api/drivers/catalog"},
		{name: "driver catalog via HEAD", method: http.MethodHead, path: "/api/drivers/catalog"},
		{name: "driver detail", method: http.MethodGet, path: "/api/drivers/sonnen"},
		{name: "driver detail via HEAD", method: http.MethodHead, path: "/api/drivers/sonnen"},
		{name: "driver source", method: http.MethodGet, path: "/api/drivers/sonnen/source"},
		{name: "driver source via HEAD", method: http.MethodHead, path: "/api/drivers/sonnen/source"},
		{name: "driver logs", method: http.MethodGet, path: "/api/drivers/sonnen/logs"},
		{name: "driver logs via HEAD", method: http.MethodHead, path: "/api/drivers/sonnen/logs"},
		{name: "device repository status", method: http.MethodGet, path: "/api/device_repository/status"},
		{name: "device repository status via HEAD", method: http.MethodHead, path: "/api/device_repository/status"},
		{name: "component status", method: http.MethodGet, path: "/api/components"},
		{name: "component status via HEAD", method: http.MethodHead, path: "/api/components"},
		{name: "component history", method: http.MethodGet, path: "/api/components/history"},
		{name: "component history via HEAD", method: http.MethodHead, path: "/api/components/history"},
		{name: "Home Assistant status", method: http.MethodGet, path: "/api/ha/status"},
		{name: "Home Assistant status via HEAD", method: http.MethodHead, path: "/api/ha/status"},
		{name: "CalDAV status", method: http.MethodGet, path: "/api/caldav/status"},
		{name: "CalDAV status via HEAD", method: http.MethodHead, path: "/api/caldav/status"},
		{name: "notification status", method: http.MethodGet, path: "/api/notifications/status"},
		{name: "notification status via HEAD", method: http.MethodHead, path: "/api/notifications/status"},
		{name: "notification history", method: http.MethodGet, path: "/api/notifications/history"},
		{name: "notification history via HEAD", method: http.MethodHead, path: "/api/notifications/history"},
		{name: "version snapshots", method: http.MethodGet, path: "/api/version/snapshots"},
		{name: "version snapshots via HEAD", method: http.MethodHead, path: "/api/version/snapshots"},
		{name: "driver list", method: http.MethodGet, path: "/api/drivers"},
		{name: "driver list via HEAD", method: http.MethodHead, path: "/api/drivers"},
		{name: "driver draft", method: http.MethodGet, path: "/api/drivers/sonnen/draft"},
		{name: "EV status", method: http.MethodGet, path: "/api/ev/status"},
		{name: "EV status via HEAD", method: http.MethodHead, path: "/api/ev/status"},
		{name: "series", method: http.MethodGet, path: "/api/series"},
		{name: "series catalog", method: http.MethodGet, path: "/api/series/catalog"},
		{name: "planner diagnose", method: http.MethodGet, path: "/api/mpc/diagnose"},
		{name: "planner diagnose history", method: http.MethodGet, path: "/api/mpc/diagnose/history"},
		{name: "planner diagnose at", method: http.MethodGet, path: "/api/mpc/diagnose/at"},
		{name: "fleet ping", method: http.MethodGet, path: "/api/fleet-ping"},
		{name: "notification rules", method: http.MethodGet, path: "/api/notifications/rules"},
		{name: "device repository catalog", method: http.MethodGet, path: "/api/device_repository/catalog"},
		{name: "device repository versions", method: http.MethodGet, path: "/api/device_repository/drivers/sonnen/versions"},
		{name: "app-link status", method: http.MethodGet, path: "/api/app-link/status"},
	}

	for _, tc := range guarded {
		t.Run(tc.name+" blocks cross-site", func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://ftw.local:8080"+tc.path, nil)
			req.RemoteAddr = "192.168.1.10:43210"
			req.Header.Set("Origin", "https://attacker.example")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rr := httptest.NewRecorder()
			Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
			}
		})

		t.Run(tc.name+" allows same-origin LAN", func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://ftw.local:8080"+tc.path, nil)
			req.RemoteAddr = "192.168.1.10:43210"
			req.Header.Set("Origin", "http://ftw.local:8080")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			rr := httptest.NewRecorder()
			Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true}).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestAuthenticateLeavesOrdinaryReadsAndOAuthCallbackCompatible(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/status"},
		{method: http.MethodHead, path: "/api/status"},
		{method: http.MethodOptions, path: "/api/status"},
		{method: http.MethodGet, path: "/api/version/check"},
		{method: http.MethodGet, path: "/api/oauth/myuplink/callback?code=code&state=state"},
		{method: http.MethodGet, path: "/api/energy/history"},
		{method: http.MethodGet, path: "/api/prices"},
		{method: http.MethodGet, path: "/api/mpc/plan"},
		// Dashboard poll — must stay open so lan_auth does not pop a login
		// on every 2s status tick.
		{method: http.MethodGet, path: "/api/loadpoints"},
		{method: http.MethodGet, path: "/api/history"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://ftw.local:8080"+tc.path, nil)
			req.RemoteAddr = "192.168.1.10:43210"
			req.Header.Set("Origin", "https://identity.example")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rr := httptest.NewRecorder()
			Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAuthenticateRequiresRemoteTokenForProtectedReads(t *testing.T) {
	policy := MutationPolicy{RequireTokenForRemote: true, Token: testMutationToken}
	for _, path := range []string{
		"/api/scan",
		"/api/backups",
		"/api/backups/ftw-full-backup-20260731T120000Z.ftwbak",
		"/api/caldav/credentials",
		"/api/config",
		"/api/support/dump",
		"/api/support/report",
		"/api/assistant/status",
		"/api/assistant/threads",
		"/api/assistant/threads/aaaa000000000001",
		"/api/logs",
		"/api/system/info",
		"/api/storage/inventory",
		"/api/research/load/dump",
		"/api/app-link/devices",
		"/api/devices",
		"/api/drivers/catalog",
		"/api/drivers/sonnen",
		"/api/drivers/sonnen/source",
		"/api/drivers/sonnen/logs",
		"/api/device_repository/status",
		"/api/components",
		"/api/components/history",
		"/api/ha/status",
		"/api/caldav/status",
		"/api/notifications/status",
		"/api/notifications/history",
		"/api/notifications/rules",
		"/api/version/snapshots",
		"/api/drivers",
		"/api/drivers/sonnen/draft",
		"/api/ev/status",
		"/api/series",
		"/api/series/catalog",
		"/api/mpc/diagnose",
		"/api/mpc/diagnose/history",
		"/api/fleet-ping",
		"/api/device_repository/catalog",
		"/api/device_repository/drivers/sonnen/versions",
		"/api/app-link/status",
	} {
		t.Run(path, func(t *testing.T) {
			request := func(auth string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "https://ftw.example.com"+path, nil)
				req.RemoteAddr = "192.168.1.10:43210"
				req.Header.Set("Origin", "https://ftw.example.com")
				req.Header.Set("Sec-Fetch-Site", "same-origin")
				if auth != "" {
					req.Header.Set("Authorization", auth)
				}
				rr := httptest.NewRecorder()
				Authenticate(statusHandler(http.StatusNoContent), policy).ServeHTTP(rr, req)
				return rr
			}

			if rr := request(""); rr.Code != http.StatusUnauthorized {
				t.Fatalf("missing token status = %d, want 401", rr.Code)
			} else if got := rr.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("missing token Cache-Control = %q, want no-store", got)
			}
			if rr := request("Bearer " + testMutationToken); rr.Code != http.StatusNoContent {
				t.Fatalf("valid token status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAuthenticateAllowsSameOriginAndLocalCLIFlows(t *testing.T) {
	for _, endpoint := range sensitiveMutations {
		t.Run(endpoint.name+" same-origin browser", func(t *testing.T) {
			h := Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true})
			req := mutationRequest(endpoint, "http://ftw.local:8080")
			req.Header.Set("Origin", "http://ftw.local:8080")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
			}
		})

		t.Run(endpoint.name+" private-address CLI", func(t *testing.T) {
			h := Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true})
			req := mutationRequest(endpoint, "http://192.168.1.42:8080")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAuthenticateRequiresBearerTokenForRemoteHost(t *testing.T) {
	policy := MutationPolicy{RequireTokenForRemote: true, Token: testMutationToken}
	request := func(auth string) *httptest.ResponseRecorder {
		req := mutationRequest(sensitiveMutations[0], "https://ftw.example.com")
		req.Header.Set("Origin", "https://ftw.example.com")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		Authenticate(statusHandler(http.StatusNoContent), policy).ServeHTTP(rr, req)
		return rr
	}

	if rr := request(""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rr.Code)
	}
	if rr := request("Bearer wrong"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d, want 401", rr.Code)
	}
	if rr := request("Bearer " + testMutationToken); rr.Code != http.StatusNoContent {
		t.Fatalf("valid token status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}

	locked := MutationPolicy{RequireTokenForRemote: true}
	req := mutationRequest(sensitiveMutations[0], "https://ftw.example.com")
	req.Header.Set("Origin", "https://ftw.example.com")
	rr := httptest.NewRecorder()
	Authenticate(statusHandler(http.StatusNoContent), locked).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unconfigured remote policy status = %d, want 403", rr.Code)
	}
}

func TestAuthenticateNoDotHostIsNotLocal(t *testing.T) {
	req := mutationRequest(sensitiveMutations[4], "http://intranet:8080")
	req.RemoteAddr = "192.168.1.10:43210"
	req.Header.Set("Origin", "http://intranet:8080")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true}).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("no-dot host status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestAuthenticateRemoteClientCannotSpoofLocalHost(t *testing.T) {
	req := mutationRequest(sensitiveMutations[4], "http://192.168.1.42:8080")
	req.RemoteAddr = "203.0.113.10:43210"
	rr := httptest.NewRecorder()
	Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true}).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestAuthenticateRequiresJSONContentTypeForBodies(t *testing.T) {
	for _, endpoint := range sensitiveMutations {
		if endpoint.body == "" {
			continue
		}
		t.Run(endpoint.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://ftw.local:8080"+endpoint.path, strings.NewReader(endpoint.body))
			rr := httptest.NewRecorder()
			Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("missing Content-Type status = %d, want 415", rr.Code)
			}

			req = mutationRequest(endpoint, "http://ftw.local:8080")
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			rr = httptest.NewRecorder()
			Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("JSON Content-Type status = %d, want 204", rr.Code)
			}
		})
	}
}

func TestAuthenticateRejectsOriginMismatchEvenWhenFetchSiteClaimsSameOrigin(t *testing.T) {
	req := mutationRequest(sensitiveMutations[1], "http://192.168.1.42:8080")
	req.Header.Set("Origin", "http://192.168.1.99:8080")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestAuthenticateRejectsInvalidHostAndFetchMetadata(t *testing.T) {
	tests := []struct {
		name       string
		host       string
		origin     string
		fetchSite  string
		wantStatus int
	}{
		{name: "invalid Host", host: "bad host", wantStatus: http.StatusBadRequest},
		{name: "null Origin", host: "ftw.local:8080", origin: "null", wantStatus: http.StatusForbidden},
		{name: "same-site fetch without Origin", host: "ftw.local:8080", fetchSite: "same-site", wantStatus: http.StatusForbidden},
		{name: "cross-site fetch without Origin", host: "ftw.local:8080", fetchSite: "cross-site", wantStatus: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := mutationRequest(sensitiveMutations[4], "http://ftw.local:8080")
			req.Host = tc.host
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			rr := httptest.NewRecorder()
			Authenticate(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
			if rr.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rr.Code, tc.wantStatus)
			}
		})
	}
}

func TestServerHandlerAppliesMutationSecurity(t *testing.T) {
	srv := New(&Deps{MutationPolicy: MutationPolicy{RequireTokenForRemote: true}})
	req := httptest.NewRequest(http.MethodPost, "https://ftw.example.com/api/restart", nil)
	req.Header.Set("Origin", "https://ftw.example.com")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "http://ftw.local:8080/api/scan", nil)
	req.RemoteAddr = "192.168.1.10:43210"
	req.Header.Set("Origin", "https://attacker.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-site scan status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestServerHandlerSetsSecurityHeaders(t *testing.T) {
	srv := New(&Deps{})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	assertSecurityHeaders(t, rr)
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func TestWithSecurityHeadersAppliesOnEveryStatus(t *testing.T) {
	h := WithSecurityHeaders(statusHandler(http.StatusNotFound))
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	assertSecurityHeaders(t, rr)
}

func TestJSONResponsesDoNotAdvertiseWildcardCORS(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSON(rr, http.StatusOK, map[string]string{"status": "ok"})
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

func mutationRequest(endpoint sensitiveMutation, baseURL string) *http.Request {
	var body *strings.Reader
	if endpoint.body != "" {
		body = strings.NewReader(endpoint.body)
	} else {
		body = strings.NewReader("")
	}
	req := httptest.NewRequest(http.MethodPost, baseURL+endpoint.path, body)
	req.RemoteAddr = "192.168.1.10:43210"
	if endpoint.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func statusHandler(status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
}

func assertSecurityHeaders(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	}
	for name, value := range want {
		if got := rr.Header().Get(name); got != value {
			t.Errorf("%s = %q, want %q", name, got, value)
		}
	}
}
