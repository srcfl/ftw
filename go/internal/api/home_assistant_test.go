package api

import (
	"github.com/srcfl/ftw/go/internal/apiauth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testIngressPrefix = "/api/hassio_ingress/valid_token"

func ingressRequest(method, path string) *http.Request {
	r := httptest.NewRequest(method, "https://ha.example"+path, nil)
	r.RemoteAddr = "172.30.32.2:1234"
	r.Header.Set("X-Ingress-Path", testIngressPrefix)
	r.Header.Set("X-Remote-User-Id", "owner-id")
	r.Header.Set("Origin", "https://ha.example")
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	return r
}

func TestIngressOnlyTrustsSupervisor(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !HomeAssistantIngress(r) {
			t.Fatal("missing trusted context")
		}
		caller, _ := apiauth.FromRequest(r)
		if caller.Role != apiauth.RoleOwner || caller.Subject != "ha:owner-id" {
			t.Fatalf("caller: %+v", caller)
		}
		if hasForwardingHeader(r.Header) {
			t.Fatal("forwarding metadata reached local gates")
		}
		w.WriteHeader(204)
	})
	policy := MutationPolicy{RequireTokenForRemote: true, LANAuthEnabled: func() bool { return true }, VerifyLANSecret: func(string) bool { return false }}
	handler := WithHomeAssistantIngress(WithSecurityHeaders(Authenticate(next, policy)))
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"HA owner with an existing password", func(r *http.Request) {}, 204},
		{"LAN spoofing all headers", func(r *http.Request) { r.RemoteAddr = "192.168.1.50:1234" }, 403},
		{"loopback cannot impersonate HA", func(r *http.Request) { r.RemoteAddr = "127.0.0.1:1234" }, 403},
		{"missing user", func(r *http.Request) { r.Header.Del("X-Remote-User-Id") }, 403},
		{"missing prefix", func(r *http.Request) { r.Header.Del("X-Ingress-Path") }, 403},
		{"invalid prefix", func(r *http.Request) { r.Header.Set("X-Ingress-Path", `/api/hassio_ingress/evil"><script>`) }, 403},
		{"cross-site request", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, 403},
		{"cross-site fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ingressRequest("POST", "/api/config")
			tc.change(r)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if w.Code == 204 && w.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
				t.Fatal("HA iframe blocked")
			}
		})
	}
	denied := httptest.NewRecorder()
	ordinary := Authenticate(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("spoof accepted") }), policy)
	r := ingressRequest("POST", "/api/config")
	r.Host = "ftw.local"
	r.Header.Set("Origin", "http://ftw.local")
	ordinary.ServeHTTP(denied, r)
	if denied.Code != 401 {
		t.Fatalf("ordinary listener status %d", denied.Code)
	}
}

func TestIngressHealthDoesNotExposeUI(t *testing.T) {
	handler := WithHomeAssistantIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, path := range []string{"/api/health", "/setup", "/api/config", "/"} {
		r := httptest.NewRequest("GET", "http://localhost"+path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := 403
		if path == "/api/health" {
			want = 204
		}
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestIngressAdaptsStaticURLsButNeverAPIData(t *testing.T) {
	asset := `<head></head><script src="/app.js"></script><a href="/setup">Setup</a><a href="/">Home</a>` +
		"fetch('/api/config'); import('/components/index.js'); window.location.href = '/'; path.split('/'); url + '/draft'; '//external';"
	result := string(ingressAsset([]byte(asset), testIngressPrefix, true))
	for _, path := range []string{"/app.js", "/setup", "/api/config", "/components/index.js", "/home-assistant.js"} {
		if !strings.Contains(result, testIngressPrefix+path) {
			t.Errorf("missing prefix for %s: %s", path, result)
		}
	}
	for _, unchanged := range []string{"path.split('/')", "url + '/draft'", "'//external'"} {
		if !strings.Contains(result, unchanged) {
			t.Errorf("changed non-root URL: %s", unchanged)
		}
	}
	original := `{"name":"/api/config","location":"/setup"}`
	handler := WithHomeAssistantIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(original))
	}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, ingressRequest("GET", "/api/config"))
	if w.Body.String() != original {
		t.Fatal("API data rewritten")
	}
}

func TestIngressRedirectStaysUnderHA(t *testing.T) {
	handler := WithHomeAssistantIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/setup", 302) }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, ingressRequest("GET", "/"))
	if w.Header().Get("Location") != testIngressPrefix+"/setup" {
		t.Fatal(w.Header())
	}
}

func TestIngressKeepsAnExistingStoredPassword(t *testing.T) {
	resetLANGuesses(t)
	server := newLANAuthServer(t)
	enableStoredLANAuth(t, server)
	before := server.deps.Cfg.LANPasswordHash
	if before == "" {
		t.Fatal("test needs a real stored password")
	}
	handler := WithHomeAssistantIngress(server.Handler())
	for _, path := range []string{"/api/config", "/api/auth/status"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, ingressRequest("GET", path))
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if server.deps.Cfg.LANPasswordHash != before {
		t.Fatal("ingress changed the saved password")
	}
}

func TestIngressVendorCallbackKeepsTheExternalHAURL(t *testing.T) {
	handler := WithHomeAssistantIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callback := requestOrigin(r) + myUplinkCallbackPath
		if callback != "https://ha.example"+testIngressPrefix+myUplinkCallbackPath {
			t.Fatal("wrong callback", callback)
		}
		if !validMyUplinkCallback(callback) {
			t.Fatal("HA callback refused")
		}
	}))
	req := ingressRequest("GET", "/api/oauth/myuplink/start")
	req.Header.Set("X-Forwarded-Proto", "https")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	for _, bad := range []string{"/setup", "?code=wrong", "#fragment"} {
		if validMyUplinkCallback("https://ha.example" + testIngressPrefix + myUplinkCallbackPath + bad) {
			t.Fatal("invalid callback accepted")
		}
	}
}

func TestIngressAllowsTheNormalVendorOAuthReturn(t *testing.T) {
	// An OAuth browser navigation arrives from the vendor's origin. The nonce
	// protects the callback; the existing mutation policy exempts this GET.
	req := ingressRequest("GET", myUplinkCallbackPath+"?code=example&state=example")
	req.Header.Del("Origin")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if requiresMutationProtection(req) {
		t.Fatal("callback no longer uses its own state proof")
	}
	handler := WithHomeAssistantIngress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 {
		t.Fatalf("vendor return blocked: %d", w.Code)
	}
}
