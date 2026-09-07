package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

const testHousePassword = "house-pass-ok"

func resetLANSessions() {
	lanSessionMu.Lock()
	lanSessions = map[string]lanSession{}
	lanSessionNow = time.Now
	lanSessionMu.Unlock()
}

func resetLANGuesses(t *testing.T) {
	t.Helper()
	lanGuessMu.Lock()
	lanGuessFailures = 0
	lanGuessLockedUntil = time.Time{}
	lanGuessNow = time.Now
	lanGuessMu.Unlock()
	resetLANSessions()
	t.Cleanup(func() {
		lanGuessMu.Lock()
		lanGuessFailures = 0
		lanGuessLockedUntil = time.Time{}
		lanGuessNow = time.Now
		lanGuessMu.Unlock()
		resetLANSessions()
	})
}

func mustIssueLANSession(t *testing.T) string {
	t.Helper()
	token, err := issueLANSession()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func lanSessionFromRecorder(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rr.Result().Cookies() {
		if c.Name == lanSessionCookieName && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("missing %s cookie: %q", lanSessionCookieName, rr.Header().Get("Set-Cookie"))
	return ""
}

func enableStoredLANAuth(t *testing.T, srv *Server) {
	t.Helper()
	body := `{"password":"` + testHousePassword + `","enabled":true}`
	post := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/auth/password", strings.NewReader(body))
	post.RemoteAddr = "127.0.0.1:43210"
	post.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, post)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rr.Code, rr.Body.String())
	}
}

func postLANAuthJSON(srv *Server, method, url, remote, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.RemoteAddr = remote
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

func lanAuthPolicy(secret string) MutationPolicy {
	return MutationPolicy{
		LANAuthEnabled: func() bool { return true },
		VerifyLANSecret: func(got string) bool {
			return got == secret
		},
	}
}

func lanAuthRequest(method, url, remote, auth string) *http.Request {
	req := httptest.NewRequest(method, url, nil)
	req.RemoteAddr = remote
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}

func serveLANAuth(policy MutationPolicy, req *http.Request, peek func(*http.Request)) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if peek != nil {
			peek(r)
		}
		w.WriteHeader(http.StatusNoContent)
	}), policy).ServeHTTP(rr, req)
	return rr
}

func TestLANAuthProtectedConfigRequiresBearer(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "")
	rr := serveLANAuth(policy, req, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != `Bearer realm="ftw-lan"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	if !strings.Contains(rr.Body.String(), "valid LAN password required") {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestLANAuthProtectedConfigAcceptsHouseBearer(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	var caller apiauth.Caller
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "Bearer "+testHousePassword)
	rr := serveLANAuth(policy, req, func(r *http.Request) {
		caller, _ = apiauth.FromRequest(r)
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if caller.Kind != apiauth.KindLAN || caller.Role != apiauth.RoleOwner {
		t.Fatalf("caller = %+v, want LAN owner", caller)
	}
	if !caller.Scopes.Has("ftw.dispatch.write") {
		t.Fatal("owner lost unrestricted scopes")
	}
}

func TestLANAuthProtectedConfigRejectsWrongBearer(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "Bearer wrong-password")
	rr := serveLANAuth(policy, req, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestLANAuthUnprotectedReadIsViewer(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	var caller apiauth.Caller
	var called bool
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/status", "192.168.1.10:43210", "")
	rr := serveLANAuth(policy, req, func(r *http.Request) {
		called = true
		caller, _ = apiauth.FromRequest(r)
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if !called {
		t.Fatal("unprotected handler was not called")
	}
	if caller.Kind != apiauth.KindLAN || caller.Role != apiauth.RoleViewer {
		t.Fatalf("caller = %+v, want LAN viewer", caller)
	}
	if caller.Scopes.Has("ftw.dispatch.write") {
		t.Fatal("viewer was handed owner scopes")
	}
	if !caller.Scopes.Has("ftw.live.read") {
		t.Fatal("viewer lost its live-read scope")
	}
}

func TestLANAuthLoopbackSkipsHousePassword(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	var caller apiauth.Caller
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "127.0.0.1:43210", "")
	rr := serveLANAuth(policy, req, func(r *http.Request) {
		caller, _ = apiauth.FromRequest(r)
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if caller.Role != apiauth.RoleOwner {
		t.Fatalf("loopback caller = %+v, want owner", caller)
	}

	req = lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "[::1]:43210", "")
	rr = serveLANAuth(policy, req, func(r *http.Request) {
		caller, _ = apiauth.FromRequest(r)
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("::1 status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if caller.Role != apiauth.RoleOwner {
		t.Fatalf("::1 caller = %+v, want owner", caller)
	}
}

func TestLANAuthDoesNotReplaceAppCaller(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	app := apiauth.Caller{
		Subject: "app:phone1",
		Kind:    apiauth.KindApp,
		Role:    apiauth.RoleOwner,
		Scopes:  apiauth.NewScopeSet(apiauth.RoleScopes[apiauth.RoleOwner]...),
	}
	var got apiauth.Caller
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "")
	req = req.WithContext(apiauth.WithCaller(req.Context(), app))
	rr := serveLANAuth(policy, req, func(r *http.Request) {
		got, _ = apiauth.FromRequest(r)
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if got.Kind != apiauth.KindApp || got.Subject != app.Subject {
		t.Fatalf("caller = %+v, want the app session", got)
	}
}

func TestLANAuthOffKeepsOpenLAN(t *testing.T) {
	resetLANGuesses(t)
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "")
	rr := serveLANAuth(MutationPolicy{}, req, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestPrivateLANAddressIsNotLoopback(t *testing.T) {
	if isLoopbackClient("192.168.1.10:43210") {
		t.Fatal("192.168.1.10 treated as loopback")
	}
	if isLoopbackClient("10.0.0.5:80") {
		t.Fatal("10.0.0.5 treated as loopback")
	}
	if isLoopbackClient("172.16.1.1:80") {
		t.Fatal("172.16.1.1 treated as loopback")
	}
	if !isLoopbackClient("127.0.0.1:43210") {
		t.Fatal("127.0.0.1 is loopback")
	}
	if !isLoopbackClient("[::1]:43210") {
		t.Fatal("::1 is loopback")
	}
}

func TestLANAuthGuessLockout(t *testing.T) {
	resetLANGuesses(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	lanGuessNow = func() time.Time { return now }
	policy := lanAuthPolicy(testHousePassword)

	for i := 0; i < lanGuessLimit; i++ {
		req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "Bearer wrong")
		rr := serveLANAuth(policy, req, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d status = %d, want 401", i+1, rr.Code)
		}
	}
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "Bearer "+testHousePassword)
	rr := serveLANAuth(policy, req, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("locked correct password status = %d, want 401", rr.Code)
	}

	now = now.Add(lanGuessCooldown + time.Second)
	rr = serveLANAuth(policy, req, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("after cooldown status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestLANAuthStatusIsReadableWithoutPassword(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/auth/status", "192.168.1.10:43210", "")
	rr := serveLANAuth(policy, req, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestLANAuthLoginLogoutAreExempt(t *testing.T) {
	resetLANGuesses(t)
	policy := lanAuthPolicy(testHousePassword)
	for _, path := range []string{"/api/auth/login", "/api/auth/logout"} {
		req := httptest.NewRequest(http.MethodPost, "http://ftw.local:8080"+path, strings.NewReader(`{}`))
		req.RemoteAddr = "192.168.1.10:43210"
		req.Header.Set("Content-Type", "application/json")
		rr := serveLANAuth(policy, req, nil)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("%s status = %d, want 204 (body=%s)", path, rr.Code, rr.Body.String())
		}
	}
}

func TestLANAuthDoesNotOverrideRemoteToken(t *testing.T) {
	resetLANGuesses(t)
	policy := MutationPolicy{
		RequireTokenForRemote: true,
		Token:                 testMutationToken,
		LANAuthEnabled:        func() bool { return true },
		VerifyLANSecret:       func(string) bool { return false },
	}
	req := httptest.NewRequest(http.MethodGet, "https://ftw.example.com/api/config", nil)
	req.RemoteAddr = "203.0.113.10:43210"
	req.Header.Set("Origin", "https://ftw.example.com")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Authorization", "Bearer "+testMutationToken)
	rr := serveLANAuth(policy, req, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("public host + API token status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestLANAuthNilVerifyIsOff(t *testing.T) {
	resetLANGuesses(t)
	policy := MutationPolicy{LANAuthEnabled: func() bool { return true }}
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "Bearer "+testHousePassword)
	rr := serveLANAuth(policy, req, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when VerifyLANSecret is nil", rr.Code)
	}
}

func newLANAuthServer(t *testing.T) *Server {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{API: config.API{Port: 8080}, Site: config.Site{SmoothingAlpha: .3}, Fuse: config.Fuse{MaxAmps: 16, Phases: 3, Voltage: 230}}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err = config.InitializeStorage(cfgPath, statePath, cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&Deps{
		State:      st,
		Cfg:        cfg,
		CfgMu:      &sync.RWMutex{},
		ConfigPath: cfgPath,
		SaveConfig: func(path string, cfg *config.Config) error { return config.SaveStored(st, path, cfg) },
		WebDir:     t.TempDir(),
		MutationPolicy: MutationPolicy{
			LANAuthEnabled: func() bool {
				return cfg.API.LANAuth
			},
			VerifyLANSecret: func(secret string) bool {
				return VerifyStoredLANSecret(st, secret)
			},
		},
	})
	return srv
}

func TestAuthPasswordEnableAndStatus(t *testing.T) {
	resetLANGuesses(t)
	srv := newLANAuthServer(t)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/auth/status", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("initial status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var status struct {
		LANAuth    bool `json:"lan_auth"`
		Configured bool `json:"configured"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.LANAuth || status.Configured {
		t.Fatalf("initial status = %+v, want both false", status)
	}

	empty := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/auth/password", strings.NewReader(`{"password":"","enabled":true}`))
	empty.RemoteAddr = "127.0.0.1:43210"
	empty.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, empty)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("empty enable status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}

	short := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/auth/password", strings.NewReader(`{"password":"short","enabled":true}`))
	short.RemoteAddr = "127.0.0.1:43210"
	short.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, short)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("short enable status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}

	body := `{"password":"` + testHousePassword + `","enabled":true}`
	lanEnable := httptest.NewRequest(http.MethodPost, "http://ftw.local:8080/api/auth/password", strings.NewReader(body))
	lanEnable.RemoteAddr = "192.168.1.10:43210"
	lanEnable.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, lanEnable)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("first LAN enable status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}

	// Docker Desktop published ports SNAT to the bridge gateway, not
	// loopback. That peer is RFC1918, same as a LAN visitor.
	bridge := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/auth/password", strings.NewReader(body))
	bridge.RemoteAddr = "172.17.0.1:43210"
	bridge.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, bridge)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("docker-bridge enable status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "docker compose") {
		t.Fatalf("403 body should name the Docker Desktop exec path, got %s", rr.Body.String())
	}

	post := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/auth/password", strings.NewReader(body))
	post.RemoteAddr = "127.0.0.1:43210"
	post.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, post)
	if rr.Code != http.StatusOK {
		t.Fatalf("loopback enable status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/auth/status", nil)
	req.RemoteAddr = "192.168.1.10:43210"
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("enabled status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.LANAuth || !status.Configured {
		t.Fatalf("enabled status = %+v, want both true", status)
	}

	blocked := httptest.NewRequest(http.MethodPost, "http://ftw.local:8080/api/auth/password", strings.NewReader(`{"enabled":false}`))
	blocked.RemoteAddr = "192.168.1.10:43210"
	blocked.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, blocked)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("LAN disable without secret status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}

	encoded, ok := srv.deps.State.LoadConfig(lanAuthPasswordKey)
	if !ok || !strings.Contains(encoded, "$argon2id$") {
		t.Fatalf("stored hash = %q, want argon2id encoding", encoded)
	}
}

func TestPostConfigCannotEnableLANAuth(t *testing.T) {
	srv, _, cfg := postConfigServer(t, nil)
	body := strings.Replace(firstSiteMeterConfig, `"api": {"port": 8080}`, `"api": {"port": 8080, "lan_auth": true}`, 1)
	if code := postConfig(t, srv, body); code != 200 {
		t.Fatalf("POST /api/config = %d, want 200", code)
	}
	if cfg.API.LANAuth {
		t.Fatal("config POST turned lan_auth on; only /api/auth/password may do that")
	}
}

func TestGetConfigDoesNotContainLANPasswordHash(t *testing.T) {
	resetLANGuesses(t)
	srv := newLANAuthServer(t)
	body := `{"password":"` + testHousePassword + `","enabled":true}`
	post := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/auth/password", strings.NewReader(body))
	post.RemoteAddr = "127.0.0.1:43210"
	post.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, post)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rr.Code, rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/config", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("config status = %d, body=%s", rr.Code, rr.Body.String())
	}
	raw := rr.Body.String()
	if strings.Contains(raw, "$argon2id$") || strings.Contains(raw, testHousePassword) {
		t.Fatalf("config leaked the house secret: %s", raw)
	}
	if !strings.Contains(raw, `"lan_auth":true`) {
		t.Fatalf("config missing lan_auth flag: %s", raw)
	}

	onDisk, err := os.ReadFile(srv.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "$argon2id$") || strings.Contains(string(onDisk), testHousePassword) {
		t.Fatalf("config.yaml leaked the house secret: %s", onDisk)
	}
}

func TestAuthPasswordDisableClearsHash(t *testing.T) {
	resetLANGuesses(t)
	srv := newLANAuthServer(t)
	body := `{"password":"` + testHousePassword + `","enabled":true}`
	post := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/auth/password", strings.NewReader(body))
	post.RemoteAddr = "127.0.0.1:43210"
	post.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, post)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body=%s", rr.Code, rr.Body.String())
	}

	disable := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/auth/password", strings.NewReader(`{"enabled":false}`))
	disable.RemoteAddr = "127.0.0.1:43210"
	disable.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, disable)
	if rr.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if lanPasswordConfigured(srv.deps.State) {
		t.Fatal("hash still stored after disable")
	}
	if srv.deps.Cfg.API.LANAuth {
		t.Fatal("lan_auth still set after disable")
	}
}

func TestLANPasswordHashRoundTrip(t *testing.T) {
	encoded, err := hashLANPassword(testHousePassword)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("encoded = %q", encoded)
	}
	if !verifyLANPassword(encoded, testHousePassword) {
		t.Fatal("correct password rejected")
	}
	if verifyLANPassword(encoded, "wrong-password") {
		t.Fatal("wrong password accepted")
	}
}

func TestLANAuthProtectedConfigAcceptsSessionCookie(t *testing.T) {
	resetLANGuesses(t)
	token := mustIssueLANSession(t)
	var caller apiauth.Caller
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "")
	req.AddCookie(&http.Cookie{Name: lanSessionCookieName, Value: token})
	rr := serveLANAuth(lanAuthPolicy(testHousePassword), req, func(r *http.Request) {
		caller, _ = apiauth.FromRequest(r)
	})
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body=%s)", rr.Code, rr.Body.String())
	}
	if caller.Kind != apiauth.KindLAN || caller.Role != apiauth.RoleOwner {
		t.Fatalf("caller = %+v, want LAN owner", caller)
	}
}

func TestLANAuthProtectedConfigRejectsUnknownCookie(t *testing.T) {
	resetLANGuesses(t)
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "")
	req.AddCookie(&http.Cookie{Name: lanSessionCookieName, Value: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"})
	rr := serveLANAuth(lanAuthPolicy(testHousePassword), req, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestLANAuthBearerWinsOverSessionCookie(t *testing.T) {
	resetLANGuesses(t)
	token := mustIssueLANSession(t)
	req := lanAuthRequest(http.MethodGet, "http://ftw.local:8080/api/config", "192.168.1.10:43210", "Bearer wrong-password")
	req.AddCookie(&http.Cookie{Name: lanSessionCookieName, Value: token})
	rr := serveLANAuth(lanAuthPolicy(testHousePassword), req, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 when Bearer is present and wrong (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestAuthLoginSetsCookieAndConfigSucceeds(t *testing.T) {
	resetLANGuesses(t)
	srv := newLANAuthServer(t)
	enableStoredLANAuth(t, srv)

	rr := postLANAuthJSON(srv, http.MethodPost, "http://ftw.local:8080/api/auth/login", "192.168.1.10:43210",
		`{"password":"`+testHousePassword+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"status":"ok"`) {
		t.Fatalf("login body = %s", rr.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == lanSessionCookieName {
			cookie = c
			break
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("missing session cookie: %q", rr.Header().Get("Set-Cookie"))
	}
	if !cookie.HttpOnly {
		t.Fatal("session cookie is not HttpOnly")
	}
	if cookie.Path != "/" {
		t.Fatalf("cookie Path = %q, want /", cookie.Path)
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie SameSite = %v, want Strict", cookie.SameSite)
	}
	if cookie.Secure {
		t.Fatal("session cookie must not set Secure (LAN is http)")
	}
	if cookie.MaxAge != int(lanSessionTTL/time.Second) {
		t.Fatalf("cookie MaxAge = %d, want %d", cookie.MaxAge, int(lanSessionTTL/time.Second))
	}
	raw := rr.Header().Get("Set-Cookie")
	if strings.Contains(strings.ToLower(raw), "secure") {
		t.Fatalf("Set-Cookie advertised Secure: %q", raw)
	}

	req := httptest.NewRequest(http.MethodGet, "http://ftw.local:8080/api/config", nil)
	req.RemoteAddr = "192.168.1.10:43210"
	req.AddCookie(&http.Cookie{Name: lanSessionCookieName, Value: cookie.Value})
	got := httptest.NewRecorder()
	srv.Handler().ServeHTTP(got, req)
	if got.Code != http.StatusOK {
		t.Fatalf("config with session cookie status = %d, want 200 (body=%s)", got.Code, got.Body.String())
	}
	if !strings.Contains(got.Body.String(), `"lan_auth":true`) {
		t.Fatalf("config body = %s", got.Body.String())
	}
}

func TestAuthLoginWrongPasswordNoCookie(t *testing.T) {
	resetLANGuesses(t)
	srv := newLANAuthServer(t)
	enableStoredLANAuth(t, srv)

	rr := postLANAuthJSON(srv, http.MethodPost, "http://ftw.local:8080/api/auth/login", "192.168.1.10:43210",
		`{"password":"wrong-password"}`)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401 (body=%s)", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == lanSessionCookieName && c.Value != "" {
			t.Fatalf("wrong password set a session cookie: %+v", c)
		}
	}
}

func TestAuthLoginRejectedWhenDisabled(t *testing.T) {
	resetLANGuesses(t)
	srv := newLANAuthServer(t)
	rr := postLANAuthJSON(srv, http.MethodPost, "http://ftw.local:8080/api/auth/login", "192.168.1.10:43210",
		`{"password":"`+testHousePassword+`"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("login while off status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

func TestAuthLogoutThenConfigUnauthorized(t *testing.T) {
	resetLANGuesses(t)
	srv := newLANAuthServer(t)
	enableStoredLANAuth(t, srv)

	login := postLANAuthJSON(srv, http.MethodPost, "http://ftw.local:8080/api/auth/login", "192.168.1.10:43210",
		`{"password":"`+testHousePassword+`"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d, body=%s", login.Code, login.Body.String())
	}
	token := lanSessionFromRecorder(t, login)

	logout := httptest.NewRequest(http.MethodPost, "http://ftw.local:8080/api/auth/logout", strings.NewReader(`{}`))
	logout.RemoteAddr = "192.168.1.10:43210"
	logout.Header.Set("Content-Type", "application/json")
	logout.AddCookie(&http.Cookie{Name: lanSessionCookieName, Value: token})
	out := httptest.NewRecorder()
	srv.Handler().ServeHTTP(out, logout)
	if out.Code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200 (body=%s)", out.Code, out.Body.String())
	}
	cleared := false
	for _, c := range out.Result().Cookies() {
		if c.Name == lanSessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared && !strings.Contains(out.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout did not clear cookie: %q", out.Header().Get("Set-Cookie"))
	}

	req := httptest.NewRequest(http.MethodGet, "http://ftw.local:8080/api/config", nil)
	req.RemoteAddr = "192.168.1.10:43210"
	req.AddCookie(&http.Cookie{Name: lanSessionCookieName, Value: token})
	got := httptest.NewRecorder()
	srv.Handler().ServeHTTP(got, req)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("config after logout status = %d, want 401 (body=%s)", got.Code, got.Body.String())
	}
}
