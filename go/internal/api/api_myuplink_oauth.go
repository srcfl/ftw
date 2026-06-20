package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/config"
)

// MyUplink OAuth bootstrap.
//
// MyUplink (Azure B2C) only issues authorization-code apps via its developer
// portal — the client_credentials grant the driver originally used returns
// invalid_client (#496). This file owns the one-time browser consent so the
// operator never has to hand-paste tokens:
//
//	GET /api/oauth/myuplink/start?driver=<name>
//	  → mints a state, returns the MyUplink authorize URL + the exact
//	    redirect/callback URL the operator must register in the portal.
//	GET /api/oauth/myuplink/callback?code=&state=
//	  → exchanges the code for a refresh_token, persists it into the driver
//	    config (masked) + the state KV (so SecretOverride supersedes any
//	    stale rotated value), restarts the driver, and renders a result page.
//
// The driver then runs grant_type=refresh_token at runtime (drivers/myuplink.lua)
// and persists rotations via host.persist_secret.

// myUplinkOAuthBase is the MyUplink OAuth + API host. Overridable in tests.
var myUplinkOAuthBase = "https://api.myuplink.com"

// myUplinkScope is the least-privilege scope set; offline_access is what
// makes MyUplink return a refresh_token.
const myUplinkScope = "READSYSTEM offline_access"

// myUplinkCallbackPath is the 42w route MyUplink redirects back to. The
// operator registers <origin>+this as the Callback Url in the portal.
const myUplinkCallbackPath = "/api/oauth/myuplink/callback"

// validMyUplinkCallback guards the browser-supplied redirect_uri so our
// authorize state can't be turned into an open redirect: it must be an
// absolute http(s) URL whose path is exactly the callback route, with no
// query or fragment.
func validMyUplinkCallback(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Host == "" {
		return false
	}
	return u.Path == myUplinkCallbackPath && u.RawQuery == "" && u.Fragment == ""
}

// oauthPending is an in-flight authorization-code request, created by /start
// and consumed (once) by /callback.
type oauthPending struct {
	driver      string
	redirectURI string
	expires     time.Time
}

var (
	oauthMu       sync.Mutex
	oauthPending_ = map[string]oauthPending{} //nolint:revive // package-local state store
)

// pruneOAuthStateLocked drops expired pending entries. Caller holds oauthMu.
func pruneOAuthStateLocked(now time.Time) {
	for k, p := range oauthPending_ {
		if now.After(p.expires) {
			delete(oauthPending_, k)
		}
	}
}

// requestOrigin reconstructs the externally-visible scheme://host the browser
// used, honouring the relay's X-Forwarded-Proto. The redirect URI must match
// byte-for-byte across authorize + token exchange + portal registration, so
// it is derived once and stored with the pending state.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		// May be a comma-separated list; take the first token.
		scheme = strings.TrimSpace(strings.Split(fp, ",")[0])
	}
	return scheme + "://" + r.Host
}

// driverConfigValue reads a string config value for a named driver under
// CfgMu. Returns ("", false) if the driver or key is absent.
func (s *Server) driverConfigValue(driver, key string) (string, bool) {
	s.deps.CfgMu.RLock()
	defer s.deps.CfgMu.RUnlock()
	for i := range s.deps.Cfg.Drivers {
		if s.deps.Cfg.Drivers[i].Name == driver {
			if v, ok := s.deps.Cfg.Drivers[i].Config[key]; ok {
				if sv, ok := v.(string); ok {
					return sv, true
				}
			}
			return "", false
		}
	}
	return "", false
}

// handleMyUplinkOAuthStart: GET /api/oauth/myuplink/start?driver=<name>
func (s *Server) handleMyUplinkOAuthStart(w http.ResponseWriter, r *http.Request) {
	driver := r.URL.Query().Get("driver")
	if driver == "" {
		writeJSON(w, 400, map[string]string{"error": "driver query param required"})
		return
	}
	clientID, ok := s.driverConfigValue(driver, "client_id")
	if !ok || clientID == "" {
		writeJSON(w, 400, map[string]string{"error": "save the MyUplink Client ID for this driver first"})
		return
	}

	// The redirect URI must match byte-for-byte across the portal
	// registration, the authorize call, and the token exchange. Prefer an
	// explicit redirect_uri from the browser (location.origin) — on a
	// remote/relay origin the request Host the server sees is NOT the
	// browser's origin, so deriving it server-side would be wrong. Fall
	// back to the request origin for plain LAN access. Validate the passed
	// value so our authorize state can't be used as an open redirect.
	redirectURI := requestOrigin(r) + myUplinkCallbackPath
	if ru := r.URL.Query().Get("redirect_uri"); ru != "" {
		if !validMyUplinkCallback(ru) {
			writeJSON(w, 400, map[string]string{"error": "invalid redirect_uri (must be <origin>" + myUplinkCallbackPath + ")"})
			return
		}
		redirectURI = ru
	}

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not generate state"})
		return
	}
	stateTok := hex.EncodeToString(stateBytes)

	now := time.Now()
	oauthMu.Lock()
	pruneOAuthStateLocked(now)
	oauthPending_[stateTok] = oauthPending{
		driver:      driver,
		redirectURI: redirectURI,
		expires:     now.Add(10 * time.Minute),
	}
	oauthMu.Unlock()

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("scope", myUplinkScope)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", stateTok)
	authorizeURL := myUplinkOAuthBase + "/oauth/authorize?" + q.Encode()

	writeJSON(w, 200, map[string]string{
		"authorize_url": authorizeURL,
		"redirect_uri":  redirectURI,
		"callback":      redirectURI,
	})
}

// handleMyUplinkOAuthCallback: GET /api/oauth/myuplink/callback?code=&state=
func (s *Server) handleMyUplinkOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	code := q.Get("code")
	stateTok := q.Get("state")

	if e := q.Get("error"); e != "" {
		// MyUplink redirected back with a consent error.
		renderOAuthResult(w, false, "MyUplink returned an error: "+e+" "+q.Get("error_description"))
		return
	}

	oauthMu.Lock()
	pruneOAuthStateLocked(time.Now())
	pending, found := oauthPending_[stateTok]
	if found {
		delete(oauthPending_, stateTok) // single-use
	}
	oauthMu.Unlock()

	if !found {
		renderOAuthResult(w, false, "Invalid or expired state — start the connect again from Settings → Devices.")
		return
	}
	if code == "" {
		renderOAuthResult(w, false, "No authorization code in the callback.")
		return
	}

	clientID, _ := s.driverConfigValue(pending.driver, "client_id")
	clientSecret, _ := s.driverConfigValue(pending.driver, "client_secret")
	if clientID == "" || clientSecret == "" {
		renderOAuthResult(w, false, "Missing Client ID / Secret for driver "+pending.driver+".")
		return
	}

	refreshToken, err := s.exchangeMyUplinkCode(code, clientID, clientSecret, pending.redirectURI)
	if err != nil {
		renderOAuthResult(w, false, "Token exchange failed: "+err.Error())
		return
	}

	if err := s.persistMyUplinkRefreshToken(r, pending.driver, refreshToken); err != nil {
		renderOAuthResult(w, false, "Could not save the token: "+err.Error())
		return
	}

	renderOAuthResult(w, true, "MyUplink connected for driver \""+pending.driver+"\". You can close this tab and return to 42-watts.")
}

// exchangeMyUplinkCode swaps an authorization code for a refresh_token.
func (s *Server) exchangeMyUplinkCode(code, clientID, clientSecret, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequest("POST", myUplinkOAuthBase+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, redactToken(string(body)))
	}
	var tok struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.RefreshToken == "" {
		return "", fmt.Errorf("no refresh_token in response (is offline_access granted?)")
	}
	return tok.RefreshToken, nil
}

// persistMyUplinkRefreshToken writes the refresh_token into the driver config
// (so it's masked + visible as "saved" in the UI) AND into the state KV (so
// SecretOverride supersedes any stale rotated value), then restarts the
// driver so it picks up the token immediately.
func (s *Server) persistMyUplinkRefreshToken(r *http.Request, driver, refreshToken string) error {
	// 1. KV first — this is what SecretOverride reads at driver_init and what
	//    the runtime rotation persists to. Writing it before the config save
	//    guarantees the post-restart override matches the fresh token.
	if s.deps.State != nil {
		if err := s.deps.State.SaveConfig("driver_secret:"+driver+":refresh_token", refreshToken); err != nil {
			return err
		}
	}

	// 2. Driver config + atomic save.
	var restartCfg *config.Driver
	s.deps.CfgMu.Lock()
	for i := range s.deps.Cfg.Drivers {
		if s.deps.Cfg.Drivers[i].Name == driver {
			if s.deps.Cfg.Drivers[i].Config == nil {
				s.deps.Cfg.Drivers[i].Config = map[string]any{}
			}
			s.deps.Cfg.Drivers[i].Config["refresh_token"] = refreshToken
			c := s.deps.Cfg.Drivers[i]
			restartCfg = &c
			break
		}
	}
	var saveErr error
	if s.deps.SaveConfig != nil {
		saveErr = s.deps.SaveConfig(s.deps.ConfigPath, s.deps.Cfg)
	}
	s.deps.CfgMu.Unlock()
	if saveErr != nil {
		return saveErr
	}
	if restartCfg == nil {
		return fmt.Errorf("driver %q not found in config", driver)
	}

	// 3. Restart so the driver re-auths now (best-effort; the config watcher
	//    would also reload, but the explicit restart is deterministic).
	if s.deps.Registry != nil {
		if err := s.deps.Registry.Restart(r.Context(), *restartCfg); err != nil {
			return fmt.Errorf("driver restart: %w", err)
		}
	}
	return nil
}

// renderOAuthResult writes a minimal self-contained result page. The browser
// lands here from the MyUplink redirect, so it cannot rely on the SPA chrome.
func renderOAuthResult(w http.ResponseWriter, ok bool, msg string) {
	status := http.StatusOK
	title := "MyUplink connected"
	accent := "#1f9d57"
	if !ok {
		status = http.StatusBadRequest
		title = "MyUplink connection failed"
		accent = "#c23b3b"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `<!doctype html><html><head><meta charset="utf-8">`+
		`<meta name="viewport" content="width=device-width, initial-scale=1">`+
		`<title>`+html.EscapeString(title)+`</title>`+
		`<style>body{font-family:system-ui,-apple-system,sans-serif;background:#0f1115;color:#e8e8ea;`+
		`display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}`+
		`.card{max-width:30rem;padding:2rem;border:1px solid #2a2d34;border-radius:12px;text-align:center}`+
		`h1{font-size:1.15rem;color:`+accent+`}p{line-height:1.5;color:#b8bcc4}</style></head>`+
		`<body><div class="card"><h1>`+html.EscapeString(title)+`</h1><p>`+html.EscapeString(msg)+`</p></div></body></html>`)
}

// redactToken blanks anything that looks like a bearer/refresh token in an
// error body so secrets never reach logs or the result page.
func redactToken(s string) string {
	for _, key := range []string{"refresh_token", "access_token", "id_token"} {
		if i := strings.Index(s, key); i >= 0 {
			return s[:i] + key + "=<redacted>"
		}
	}
	return s
}
