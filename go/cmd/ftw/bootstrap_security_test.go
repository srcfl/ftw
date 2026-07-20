package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const bootstrapTestToken = "0123456789abcdef0123456789abcdef"

func TestBootstrapMutationBoundary(t *testing.T) {
	t.Setenv("FTW_API_TOKEN", "")
	tests := []struct {
		name        string
		method      string
		url         string
		origin      string
		fetchSite   string
		contentType string
		body        string
		want        int
	}{
		{
			name: "cross-site config blocked", method: http.MethodPost, url: "http://ftw.local:8080/api/config",
			origin: "https://attacker.example", fetchSite: "cross-site",
			contentType: "application/json", body: `{}`, want: http.StatusForbidden,
		},
		{
			name: "same-origin config reaches bootstrap", method: http.MethodPost, url: "http://ftw.local:8080/api/config",
			origin: "http://ftw.local:8080", fetchSite: "same-origin",
			contentType: "application/json", body: `{}`, want: http.StatusNoContent,
		},
		{
			name: "config requires JSON", method: http.MethodPost, url: "http://ftw.local:8080/api/config",
			body: `{}`, want: http.StatusUnsupportedMediaType,
		},
		{
			name: "remote update fails closed", method: http.MethodPost, url: "https://ftw.example.com/api/version/update",
			origin: "https://ftw.example.com", fetchSite: "same-origin", want: http.StatusForbidden,
		},
		{
			name: "cross-site scan blocked", method: http.MethodGet, url: "http://ftw.local:8080/api/scan",
			origin: "https://attacker.example", fetchSite: "cross-site", want: http.StatusForbidden,
		},
		{
			name: "same-origin scan reaches bootstrap", method: http.MethodGet, url: "http://ftw.local:8080/api/scan",
			origin: "http://ftw.local:8080", fetchSite: "same-origin", want: http.StatusNoContent,
		},
		{
			name: "cross-site forced version check blocked", method: http.MethodGet, url: "http://ftw.local:8080/api/version/check?force=1",
			origin: "https://attacker.example", fetchSite: "cross-site", want: http.StatusForbidden,
		},
		{
			name: "cached version read stays compatible", method: http.MethodGet, url: "http://ftw.local:8080/api/version/check",
			origin: "https://attacker.example", fetchSite: "cross-site", want: http.StatusNoContent,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.url, strings.NewReader(tc.body))
			req.RemoteAddr = "192.168.1.10:43210"
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetchSite != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			}
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rr := httptest.NewRecorder()
			secureBootstrapMutations(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestBootstrapRemoteMutationAcceptsConfiguredBearerToken(t *testing.T) {
	t.Setenv("FTW_API_TOKEN", bootstrapTestToken)
	req := httptest.NewRequest(http.MethodPost, "https://ftw.example.com/api/version/update", nil)
	req.RemoteAddr = "192.168.1.10:43210"
	req.Header.Set("Origin", "https://ftw.example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Authorization", "Bearer "+bootstrapTestToken)
	rr := httptest.NewRecorder()
	secureBootstrapMutations(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestAPIMutationPolicyRejectsShortConfiguredToken(t *testing.T) {
	t.Setenv("FTW_API_TOKEN", "too-short")
	policy := apiMutationPolicy()
	if policy.Token != "" {
		t.Fatalf("short token was accepted: %q", policy.Token)
	}
	if !policy.RequireTokenForRemote {
		t.Fatal("remote protection disabled")
	}
}
