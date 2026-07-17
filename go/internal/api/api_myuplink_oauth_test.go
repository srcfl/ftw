package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/state"
)

// buildMyUplinkOAuthServer wires a test Server with one MyUplink driver in
// config plus an in-memory state store, and returns it alongside the live
// *config.Config (so tests can read the persisted refresh_token back).
func buildMyUplinkOAuthServer(t *testing.T) (*Server, *config.Config, *state.Store) {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	cfg := &config.Config{Drivers: []config.Driver{{
		Name: "myuplink",
		Lua:  "drivers/myuplink.lua",
		Config: map[string]any{
			"client_id":     "the-client-id",
			"client_secret": "the-client-secret",
		},
	}}}
	srv := New(&Deps{
		Cfg:        cfg,
		CfgMu:      &sync.RWMutex{},
		ConfigPath: filepath.Join(t.TempDir(), "config.yaml"),
		State:      st,
		SaveConfig: func(string, *config.Config) error { return nil }, // in-memory cfg is the source of truth here
	})
	return srv, cfg, st
}

func TestNewPKCEPair(t *testing.T) {
	v, c, err := newPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Errorf("verifier length %d outside RFC 7636 range 43..128", len(v))
	}
	// challenge MUST be base64url(SHA256(verifier)), no padding.
	sum := sha256.Sum256([]byte(v))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if c != want {
		t.Errorf("challenge = %q, want S256(verifier) = %q", c, want)
	}
	if v2, _, _ := newPKCEPair(); v2 == v {
		t.Errorf("verifier not random: two calls returned %q", v)
	}
}

func TestMyUplinkOAuthStartBuildsAuthorizeURL(t *testing.T) {
	srv, _, _ := buildMyUplinkOAuthServer(t)

	req := httptest.NewRequest("GET", "http://pi.local/api/oauth/myuplink/start?driver=myuplink", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("start status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		AuthorizeURL string `json:"authorize_url"`
		RedirectURI  string `json:"redirect_uri"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	u, err := url.Parse(resp.AuthorizeURL)
	if err != nil {
		t.Fatalf("authorize_url not a URL: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "the-client-id" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type = %q, want code", q.Get("response_type"))
	}
	if q.Get("scope") != "WRITESYSTEM READSYSTEM offline_access" {
		t.Errorf("scope = %q", q.Get("scope"))
	}
	if q.Get("state") == "" {
		t.Errorf("missing state")
	}
	if q.Get("code_challenge") == "" {
		t.Errorf("missing PKCE code_challenge")
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	wantRedirect := "http://pi.local/api/oauth/myuplink/callback"
	if resp.RedirectURI != wantRedirect {
		t.Errorf("redirect_uri = %q, want %q", resp.RedirectURI, wantRedirect)
	}
	if q.Get("redirect_uri") != wantRedirect {
		t.Errorf("authorize redirect_uri = %q, want %q", q.Get("redirect_uri"), wantRedirect)
	}
}

func TestMyUplinkOAuthStartAllowsScopeOverride(t *testing.T) {
	srv, cfg, _ := buildMyUplinkOAuthServer(t)
	cfg.Drivers[0].Config["oauth_scope"] = "READSYSTEM"

	req := httptest.NewRequest("GET", "http://pi.local/api/oauth/myuplink/start?driver=myuplink", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("start status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		AuthorizeURL string `json:"authorize_url"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Scope != "READSYSTEM offline_access" {
		t.Fatalf("response scope = %q, want READSYSTEM offline_access", resp.Scope)
	}
	u, _ := url.Parse(resp.AuthorizeURL)
	if got := u.Query().Get("scope"); got != "READSYSTEM offline_access" {
		t.Fatalf("authorize scope = %q, want READSYSTEM offline_access", got)
	}
}

func TestMyUplinkOAuthStartHonoursBrowserRedirectURI(t *testing.T) {
	srv, _, _ := buildMyUplinkOAuthServer(t)

	// A browser behind a reverse proxy supplies its externally-visible callback.
	browserCb := "https://energy.example/api/oauth/myuplink/callback"
	req := httptest.NewRequest("GET", "http://pi.local/api/oauth/myuplink/start?driver=myuplink&redirect_uri="+url.QueryEscape(browserCb), nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		AuthorizeURL string `json:"authorize_url"`
		RedirectURI  string `json:"redirect_uri"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.RedirectURI != browserCb {
		t.Errorf("redirect_uri = %q, want browser-supplied %q", resp.RedirectURI, browserCb)
	}
	u, _ := url.Parse(resp.AuthorizeURL)
	if u.Query().Get("redirect_uri") != browserCb {
		t.Errorf("authorize redirect_uri = %q, want %q", u.Query().Get("redirect_uri"), browserCb)
	}
}

func TestMyUplinkOAuthStartRejectsBadRedirectURI(t *testing.T) {
	srv, _, _ := buildMyUplinkOAuthServer(t)
	for _, bad := range []string{
		"https://evil.example/steal",                           // wrong path
		"javascript:alert(1)",                                  // wrong scheme
		"https://evil.example/api/oauth/myuplink/callback?x=1", // has query
	} {
		req := httptest.NewRequest("GET", "http://pi.local/api/oauth/myuplink/start?driver=myuplink&redirect_uri="+url.QueryEscape(bad), nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("redirect_uri %q: status = %d, want 400", bad, rec.Code)
		}
	}
}

func TestMyUplinkOAuthCallbackExchangesAndPersists(t *testing.T) {
	// Stub MyUplink token endpoint: assert authorization_code grant + the
	// client_secret from config, return a refresh_token.
	var gotGrant, gotRedirect, gotSecret string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		gotGrant = vals.Get("grant_type")
		gotRedirect = vals.Get("redirect_uri")
		gotSecret = vals.Get("client_secret")
		if vals.Get("code_verifier") == "" {
			t.Errorf("missing PKCE code_verifier in token exchange")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "AT",
			"refresh_token": "RT-from-consent",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()

	old := myUplinkOAuthBase
	myUplinkOAuthBase = tokenSrv.URL
	defer func() { myUplinkOAuthBase = old }()

	srv, cfg, st := buildMyUplinkOAuthServer(t)

	// First /start to mint a valid state for this driver.
	startReq := httptest.NewRequest("GET", "http://pi.local/api/oauth/myuplink/start?driver=myuplink", nil)
	startRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(startRec, startReq)
	var sresp struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	_ = json.Unmarshal(startRec.Body.Bytes(), &sresp)
	u, _ := url.Parse(sresp.AuthorizeURL)
	stateTok := u.Query().Get("state")
	if stateTok == "" {
		t.Fatal("no state minted")
	}

	// Now the callback the browser would hit after consent.
	cbReq := httptest.NewRequest("GET", "http://pi.local/api/oauth/myuplink/callback?code=AUTH-CODE&state="+stateTok, nil)
	cbRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cbRec, cbReq)

	if cbRec.Code != 200 {
		t.Fatalf("callback status = %d, body=%s", cbRec.Code, cbRec.Body.String())
	}
	if gotGrant != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", gotGrant)
	}
	if gotSecret != "the-client-secret" {
		t.Errorf("client_secret = %q, want the-client-secret", gotSecret)
	}
	if gotRedirect != "http://pi.local/api/oauth/myuplink/callback" {
		t.Errorf("redirect_uri = %q", gotRedirect)
	}
	// The refresh_token must land in the driver config...
	if got := cfg.Drivers[0].Config["refresh_token"]; got != "RT-from-consent" {
		t.Errorf("config refresh_token = %v, want RT-from-consent", got)
	}
	// ...and in the unwatched KV so SecretOverride supersedes any stale value.
	if v, ok := st.LoadConfig("driver_secret:myuplink:refresh_token"); !ok || v != "RT-from-consent" {
		t.Errorf("KV refresh_token = %q (ok=%v), want RT-from-consent", v, ok)
	}
}

func TestMyUplinkOAuthManualExchange(t *testing.T) {
	// Stub MyUplink token endpoint.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		if vals.Get("grant_type") != "authorization_code" {
			t.Errorf("grant_type = %q", vals.Get("grant_type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "AT", "refresh_token": "RT-manual", "expires_in": 3600,
		})
	}))
	defer tokenSrv.Close()
	old := myUplinkOAuthBase
	myUplinkOAuthBase = tokenSrv.URL
	defer func() { myUplinkOAuthBase = old }()

	srv, cfg, st := buildMyUplinkOAuthServer(t)

	// /start mints state.
	startReq := httptest.NewRequest("GET", "http://pi.local/api/oauth/myuplink/start?driver=myuplink", nil)
	startRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(startRec, startReq)
	var sresp struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	_ = json.Unmarshal(startRec.Body.Bytes(), &sresp)
	u, _ := url.Parse(sresp.AuthorizeURL)
	stateTok := u.Query().Get("state")

	// Operator pastes the full redirected URL (auto-callback never reached us).
	redirectURL := "http://pi.local/api/oauth/myuplink/callback?code=MANUAL-CODE&state=" + stateTok
	body, _ := json.Marshal(map[string]string{"redirect_url": redirectURL})
	exReq := httptest.NewRequest("POST", "http://pi.local/api/oauth/myuplink/exchange", strings.NewReader(string(body)))
	exReq.Header.Set("Content-Type", "application/json")
	exRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(exRec, exReq)

	if exRec.Code != 200 {
		t.Fatalf("exchange status = %d, body=%s", exRec.Code, exRec.Body.String())
	}
	if got := cfg.Drivers[0].Config["refresh_token"]; got != "RT-manual" {
		t.Errorf("config refresh_token = %v, want RT-manual", got)
	}
	if v, ok := st.LoadConfig("driver_secret:myuplink:refresh_token"); !ok || v != "RT-manual" {
		t.Errorf("KV refresh_token = %q (ok=%v), want RT-manual", v, ok)
	}
}

func TestMyUplinkOAuthManualExchangeBadState(t *testing.T) {
	srv, _, _ := buildMyUplinkOAuthServer(t)
	body, _ := json.Marshal(map[string]string{"code": "X", "state": "nope"})
	req := httptest.NewRequest("POST", "http://pi.local/api/oauth/myuplink/exchange", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("status = %d, want 400 for unknown state", rec.Code)
	}
}

func TestMyUplinkOAuthCallbackRejectsBadState(t *testing.T) {
	srv, _, _ := buildMyUplinkOAuthServer(t)
	req := httptest.NewRequest("GET", "http://pi.local/api/oauth/myuplink/callback?code=X&state=not-a-real-state", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == 200 {
		t.Errorf("expected non-200 for unknown state, got 200")
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "state") {
		t.Errorf("error page should mention state, got: %s", rec.Body.String())
	}
}
