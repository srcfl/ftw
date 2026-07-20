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

func TestSecureMutationsBlocksBrowserCrossSiteSensitiveRoutes(t *testing.T) {
	for _, endpoint := range sensitiveMutations {
		t.Run(endpoint.name, func(t *testing.T) {
			called := false
			h := SecureMutations(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestSecureMutationsTreatsEveryUnsafeHTTPMethodAsMutation(t *testing.T) {
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
			SecureMutations(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestSecureMutationsGuardsSemanticallyActiveReads(t *testing.T) {
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
	}

	for _, tc := range guarded {
		t.Run(tc.name+" blocks cross-site", func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://ftw.local:8080"+tc.path, nil)
			req.RemoteAddr = "192.168.1.10:43210"
			req.Header.Set("Origin", "https://attacker.example")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rr := httptest.NewRecorder()
			SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
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
			SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true}).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSecureMutationsLeavesOrdinaryReadsAndOAuthCallbackCompatible(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodGet, path: "/api/status"},
		{method: http.MethodHead, path: "/api/status"},
		{method: http.MethodOptions, path: "/api/status"},
		{method: http.MethodGet, path: "/api/version/check"},
		{method: http.MethodGet, path: "/api/oauth/myuplink/callback?code=code&state=state"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://ftw.local:8080"+tc.path, nil)
			req.RemoteAddr = "192.168.1.10:43210"
			req.Header.Set("Origin", "https://identity.example")
			req.Header.Set("Sec-Fetch-Site", "cross-site")
			rr := httptest.NewRecorder()
			SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSecureMutationsRequiresRemoteTokenForSemanticallyActiveRead(t *testing.T) {
	policy := MutationPolicy{RequireTokenForRemote: true, Token: testMutationToken}
	request := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "https://ftw.example.com/api/scan", nil)
		req.RemoteAddr = "192.168.1.10:43210"
		req.Header.Set("Origin", "https://ftw.example.com")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		SecureMutations(statusHandler(http.StatusNoContent), policy).ServeHTTP(rr, req)
		return rr
	}

	if rr := request(""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d, want 401", rr.Code)
	}
	if rr := request("Bearer " + testMutationToken); rr.Code != http.StatusNoContent {
		t.Fatalf("valid token status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestSecureMutationsAllowsSameOriginAndLocalCLIFlows(t *testing.T) {
	for _, endpoint := range sensitiveMutations {
		t.Run(endpoint.name+" same-origin browser", func(t *testing.T) {
			h := SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true})
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
			h := SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true})
			req := mutationRequest(endpoint, "http://192.168.1.42:8080")
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestSecureMutationsRequiresBearerTokenForRemoteHost(t *testing.T) {
	policy := MutationPolicy{RequireTokenForRemote: true, Token: testMutationToken}
	request := func(auth string) *httptest.ResponseRecorder {
		req := mutationRequest(sensitiveMutations[0], "https://ftw.example.com")
		req.Header.Set("Origin", "https://ftw.example.com")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		SecureMutations(statusHandler(http.StatusNoContent), policy).ServeHTTP(rr, req)
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
	SecureMutations(statusHandler(http.StatusNoContent), locked).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("unconfigured remote policy status = %d, want 403", rr.Code)
	}
}

func TestSecureMutationsRemoteClientCannotSpoofLocalHost(t *testing.T) {
	req := mutationRequest(sensitiveMutations[4], "http://192.168.1.42:8080")
	req.RemoteAddr = "203.0.113.10:43210"
	rr := httptest.NewRecorder()
	SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{RequireTokenForRemote: true}).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestSecureMutationsRequiresJSONContentTypeForBodies(t *testing.T) {
	for _, endpoint := range sensitiveMutations {
		if endpoint.body == "" {
			continue
		}
		t.Run(endpoint.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://ftw.local:8080"+endpoint.path, strings.NewReader(endpoint.body))
			rr := httptest.NewRecorder()
			SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("missing Content-Type status = %d, want 415", rr.Code)
			}

			req = mutationRequest(endpoint, "http://ftw.local:8080")
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
			rr = httptest.NewRecorder()
			SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusNoContent {
				t.Fatalf("JSON Content-Type status = %d, want 204", rr.Code)
			}
		})
	}
}

func TestSecureMutationsRejectsOriginMismatchEvenWhenFetchSiteClaimsSameOrigin(t *testing.T) {
	req := mutationRequest(sensitiveMutations[1], "http://192.168.1.42:8080")
	req.Header.Set("Origin", "http://192.168.1.99:8080")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rr := httptest.NewRecorder()
	SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestSecureMutationsRejectsInvalidHostAndFetchMetadata(t *testing.T) {
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
			SecureMutations(statusHandler(http.StatusNoContent), MutationPolicy{}).ServeHTTP(rr, req)
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
