package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// Relay is the in-memory state for one running relay process.
type Relay struct {
	Queue       *tunnel.Queue
	Tokens      *TokenRegistry
	Owners      *OwnerRegistry
	PollTimeout time.Duration // 0 → 25s default

	// BaseDomain enables subdomain-per-session routing. When set (e.g.
	// "fortytwowatts.com") a request whose Host is "<token>.<BaseDomain>"
	// is served as that session — the dashboard sees normal root-absolute
	// paths instead of the /h/<token>/web prefix. Empty disables Host
	// routing entirely (every request falls through to the path-based
	// control mux), which is what local/dev + the existing tests want.
	// See docs/goals/relay-subdomain-sessions.md.
	BaseDomain string
}

// reservedLabels are first-level labels under BaseDomain that are never
// treated as a session token, so wildcard DNS (*.<BaseDomain> → relay)
// can't accidentally turn the control-plane host (or the marketing site)
// into a "session". The apex itself is handled separately in sessionToken.
var reservedLabels = map[string]bool{
	"relay":   true, // control plane: /tunnel/*, /h/*, /me/*
	"www":     true,
	"subetha": true, // legacy raw-TCP relay name
}

type registerRequest struct {
	HostID       string `json:"host_id"`
	Token        string `json:"token"`
	TTLMs        int64  `json:"ttl_ms"`
	ApprovalCode string `json:"approval_code"`
	Intent       string `json:"intent,omitempty"`
	As           string `json:"as,omitempty"`
}

type registerResponse struct {
	PublicURL   string `json:"public_url"`
	ApprovalURL string `json:"approval_url"`
}

// Handler returns the HTTP handler for this relay. When BaseDomain is set
// it first inspects the Host header: "<token>.<BaseDomain>" is served as a
// session (subdomain mode); anything else (the apex, reserved labels, raw
// IPs, the control-plane host) falls through to the path-based control mux.
// The /h/<token>/... path family stays live alongside subdomain mode for
// one release so existing pair URLs keep working during the transition.
func (r *Relay) Handler() http.Handler {
	mux := r.controlMux()
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if tok, ok := r.sessionToken(req.Host); ok {
			r.serveSession(w, req, tok)
			return
		}
		mux.ServeHTTP(w, req)
	})
}

// controlMux is the path-routed surface: tunnel control plane, the legacy
// /h/<token>/... pair family, and owner remote access. Served for every
// request whose Host is not a session subdomain.
func (r *Relay) controlMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", r.healthz)
	mux.HandleFunc("GET /tunnel/{host_id}/next", r.tunnelNext)
	mux.HandleFunc("POST /tunnel/{host_id}/response/{req_id}", r.tunnelResponse)
	mux.HandleFunc("POST /tunnel/register", r.tunnelRegister)
	mux.HandleFunc("GET /tunnel/sessions/{token}/info", r.tunnelSessionInfo)
	mux.HandleFunc("GET /h/{token}", r.publicLanding)
	mux.HandleFunc("POST /h/{token}/approve", r.publicApprove)
	mux.HandleFunc("/h/{token}/mcp", r.publicMCP)
	mux.HandleFunc("/h/{token}/web/", r.publicWeb)

	// Owner remote access (Phase 3) — site-id-keyed routes that bypass
	// the pair-token flow. Host must POST /me/register on startup; the
	// /me/<site_id>/... family then tunnels to that host's long-poll
	// loop just like /h/<token>/... does for friend access. WebAuthn
	// session validation happens on the host side via the session cookie.
	mux.HandleFunc("POST /me/register", r.meRegister)
	mux.HandleFunc("/me/{site_id}/{rest...}", r.meTunnel)
	mux.HandleFunc("/me/{site_id}", r.meRoot)
	return mux
}

// sessionToken extracts the session token from a request Host when it
// matches the "<token>.<BaseDomain>" subdomain scheme. It returns ("",
// false) for the apex, reserved labels, multi-level names, non-matching
// domains (raw IPs, localhost), and when BaseDomain is unset.
//
// The relay sits behind Cloudflare, which preserves the original Host
// header, so req.Host is the name the friend's browser actually used.
func (r *Relay) sessionToken(host string) (string, bool) {
	if r.BaseDomain == "" {
		return "", false
	}
	h := host
	if i := strings.IndexByte(h, ':'); i >= 0 { // strip :port
		h = h[:i]
	}
	h = strings.ToLower(strings.TrimSuffix(h, ".")) // tolerate trailing root dot
	base := strings.ToLower(r.BaseDomain)
	if h == base {
		return "", false // apex is control plane, never a session
	}
	label, ok := strings.CutSuffix(h, "."+base)
	if !ok || label == "" || strings.Contains(label, ".") {
		// Not our domain, or a multi-level name (a.b.<base>) outside the
		// flat first-level token scheme.
		return "", false
	}
	if reservedLabels[label] {
		return "", false
	}
	return label, true
}

func (r *Relay) pollTimeout() time.Duration {
	if r.PollTimeout > 0 {
		return r.PollTimeout
	}
	return 25 * time.Second
}

func (r *Relay) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("OK\n"))
}

func (r *Relay) tunnelNext(w http.ResponseWriter, req *http.Request) {
	hostID := req.PathValue("host_id")
	tr, err := r.Queue.Poll(req.Context(), hostID, r.pollTimeout())
	if errors.Is(err, tunnel.ErrPollTimeout) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tr)
}

func (r *Relay) tunnelResponse(w http.ResponseWriter, req *http.Request) {
	reqID := req.PathValue("req_id")
	var resp tunnel.TunneledResponse
	if err := json.NewDecoder(req.Body).Decode(&resp); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.Queue.PostResponse(reqID, resp); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Relay) tunnelRegister(w http.ResponseWriter, req *http.Request) {
	var reg registerRequest
	if err := json.NewDecoder(req.Body).Decode(&reg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, err := r.Tokens.Register(TokenRegistration{
		HostID:       reg.HostID,
		Token:        reg.Token,
		TTL:          time.Duration(reg.TTLMs) * time.Millisecond,
		ApprovalCode: reg.ApprovalCode,
		Intent:       reg.Intent,
		As:           reg.As,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(registerResponse{
		PublicURL:   fmt.Sprintf("/h/%s", reg.Token),
		ApprovalURL: fmt.Sprintf("/h/%s/approve", reg.Token),
	})
}

// landingPaths are the three relative URLs the landing-page JS needs.
// They differ between path mode (/h/<token>/…) and subdomain mode (/…),
// so the template itself stays agnostic to the routing scheme — and there
// is no positional token argument to get wrong (the cause of the
// "Wrong code" landing bug).
type landingPaths struct {
	approve   string // where the code form POSTs
	dashboard string // "open the dashboard" link target
	mcp       string // path appended to location.origin for the MCP one-liner
}

// pathModeLanding builds the legacy /h/<token>/... URLs.
func pathModeLanding(tok string) landingPaths {
	return landingPaths{
		approve:   "/h/" + tok + "/approve",
		dashboard: "/h/" + tok + "/web/",
		mcp:       "/h/" + tok + "/mcp",
	}
}

// subdomainLanding builds the session-root URLs (Host = <token>.<base>).
func subdomainLanding() landingPaths {
	return landingPaths{approve: "/approve", dashboard: "/", mcp: "/mcp"}
}

func (r *Relay) renderLanding(w http.ResponseWriter, tok string, t *Token, lp landingPaths) {
	// Bump the pending-approval counter so the operator's dashboard
	// surfaces "friend opened the URL — waiting for them to enter the code".
	r.Tokens.MarkPendingHit(tok)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, landingHTML, esc(t.As()), esc(t.Intent()), t.State(), lp.approve, lp.dashboard, lp.mcp)
}

func (r *Relay) publicLanding(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	t, err := r.Tokens.Get(tok)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	r.renderLanding(w, tok, t, pathModeLanding(tok))
}

// serveSession handles every request that arrived on a session subdomain
// (<token>.<BaseDomain>). Unlike path mode, the host sees verbatim
// root-absolute paths, so the dashboard's /api/*, /style.css etc. resolve
// correctly with no prefix stripping.
func (r *Relay) serveSession(w http.ResponseWriter, req *http.Request, tok string) {
	t, err := r.Tokens.Get(tok)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	// The code form POSTs here while still pending — handle before the
	// state gate below.
	if req.Method == http.MethodPost && req.URL.Path == "/approve" {
		r.approve(w, req, tok)
		return
	}
	// Until approved, the only thing on the subdomain is the landing page.
	if t.State() == TokenPending && req.URL.Path == "/" {
		r.renderLanding(w, tok, t, subdomainLanding())
		return
	}
	// Active → tunnel the request verbatim to the host. tunnelPublic
	// re-checks token state (pending → 425, expired/revoked → 410).
	r.tunnelPublic(w, req, tok, req.URL.Path)
}

func (r *Relay) tunnelSessionInfo(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	t, err := r.Tokens.Get(tok)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(t.Snapshot())
}

func esc(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

// landingHTML is the friend-side page. The 4-digit code travels with
// the URL (shared by the host on Signal etc.); the friend types it
// here to activate the session. Same-origin POST so there are no CORS
// surprises.
//
// Format args (in order): claimed identity (host-supplied "as"), intent,
// current token state, then three relative paths (approve, dashboard,
// mcp) so the page works under both /h/<token>/… and <token>.<base>/
// without baking in a routing scheme — see landingPaths.
const landingHTML = `<!doctype html>
<html><head><title>forty-two-watts pair session</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; padding: 0 1rem; color: #222; }
  code { background: #f4f4f4; padding: .2rem .4rem; border-radius: .2rem; }
  .panel { border: 1px solid #ddd; padding: 1.5rem; border-radius: .5rem; margin-top: 2rem; }
  input[type=text] {
    font-family: ui-monospace, monospace;
    font-size: 2rem;
    letter-spacing: 0.4em;
    text-align: center;
    width: 7em;
    padding: 6px 8px;
    border: 1px solid #f0c040;
    border-radius: .4rem;
    background: #fffbea;
  }
  button {
    font: inherit;
    font-size: 1rem;
    padding: 8px 20px;
    margin-left: 8px;
    background: #f0c040;
    color: #0a0a0a;
    border: 0;
    border-radius: .3rem;
    cursor: pointer;
    font-weight: 600;
  }
  button:disabled { opacity: 0.5; cursor: default; }
  .err { color: #c00; background: #fee; border: 1px solid #fcc; padding: .5rem; border-radius: .3rem; margin: 1rem 0; }
  .ok  { color: #060; background: #efe; border: 1px solid #cfc; padding: .5rem; border-radius: .3rem; margin: 1rem 0; }
  .muted { color: #888; font-size: 0.9em; }
</style>
</head><body>
<h1>forty-two-watts pair session</h1>
<p>From: <code>%s</code></p>
<p>Intent: %s</p>

<div class="panel" id="entry">
  <p>The host has shared a <strong>4-digit code</strong> along with this URL. Enter it to activate the session.</p>
  <p class="muted">If you don't have the code, ask the host for it on the same channel they shared the URL.</p>
  <form id="approve-form">
    <input id="code" type="text" inputmode="numeric" pattern="\d{4}" maxlength="4" placeholder="0000" autocomplete="off" autofocus>
    <button id="approve-btn" type="submit">Activate</button>
  </form>
  <div id="msg"></div>
</div>

<p class="muted">State: <code id="state">%s</code></p>

<script>
const APPROVE_PATH = %q;
const DASHBOARD_PATH = %q;
const MCP_PATH = %q;
const form = document.getElementById('approve-form');
const codeInput = document.getElementById('code');
const msg = document.getElementById('msg');
const btn = document.getElementById('approve-btn');

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  msg.textContent = '';
  msg.className = '';
  const code = codeInput.value.trim();
  if (!/^\d{4}$/.test(code)) {
    msg.className = 'err';
    msg.textContent = 'Enter 4 digits.';
    return;
  }
  btn.disabled = true;
  try {
    const resp = await fetch(APPROVE_PATH, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code }),
    });
    if (resp.status === 204) {
      msg.className = 'ok';
      msg.innerHTML = 'Activated. You can now:<br><br>' +
        '<strong>Browser:</strong> <a href="' + DASHBOARD_PATH + '">open the dashboard</a><br><br>' +
        '<strong>Claude Code:</strong><br><code>claude mcp add ftw-friend --transport http ' +
        location.origin + MCP_PATH + '</code>';
      document.getElementById('state').textContent = 'active';
      btn.style.display = 'none';
      codeInput.disabled = true;
    } else {
      const body = await resp.text();
      msg.className = 'err';
      if (resp.status === 403) {
        msg.textContent = 'Wrong code. Double-check with the host — and make sure this is the URL they sent (not a forwarded one).';
      } else {
        msg.textContent = body || ('relay error: ' + resp.status);
      }
      btn.disabled = false;
    }
  } catch (err) {
    msg.className = 'err';
    msg.textContent = 'network error: ' + err.message;
    btn.disabled = false;
  }
});
</script>
</body></html>`

func (r *Relay) publicApprove(w http.ResponseWriter, req *http.Request) {
	r.approve(w, req, req.PathValue("token"))
}

// approve is the shared body for both /h/<token>/approve (path mode) and
// <token>.<base>/approve (subdomain mode).
func (r *Relay) approve(w http.ResponseWriter, req *http.Request, tok string) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.Tokens.Approve(tok, body.Code); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *Relay) publicMCP(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	r.tunnelPublic(w, req, tok, "/mcp")
}

func (r *Relay) publicWeb(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	stripped := strings.TrimPrefix(req.URL.Path, "/h/"+tok+"/web")
	if stripped == "" {
		stripped = "/"
	}
	r.tunnelPublic(w, req, tok, stripped)
}

func (r *Relay) tunnelPublic(w http.ResponseWriter, req *http.Request, tok, innerPath string) {
	t, err := r.Tokens.Get(tok)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	switch t.State() {
	case TokenPending:
		http.Error(w, "session pending host approval", http.StatusTooEarly)
		return
	case TokenExpired, TokenRevoked:
		http.Error(w, "session expired", http.StatusGone)
		return
	}
	// Preserve the query string so the dashboard's /api/history?range=…
	// style requests reach the host intact.
	if q := req.URL.RawQuery; q != "" {
		innerPath += "?" + q
	}
	body, _ := readBody(req.Body)
	resp, err := r.Queue.Enqueue(req.Context(), t.HostID(), tunnel.TunneledRequest{
		Method: req.Method,
		Path:   innerPath,
		Header: req.Header,
		Body:   body,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	// Bump activity timestamp so the dashboard's presence indicator
	// can show "friend active 12s ago" instead of the misleading
	// "no friend connected" we had during the Phase 1+2 deploy.
	r.Tokens.TouchActivity(tok)
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func readBody(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	return io.ReadAll(r)
}

// ---- Owner remote access (Phase 3) ----

type meRegisterRequest struct {
	SiteID string `json:"site_id"`
	HostID string `json:"host_id"`
}

func (r *Relay) meRegister(w http.ResponseWriter, req *http.Request) {
	var reg meRegisterRequest
	if err := json.NewDecoder(req.Body).Decode(&reg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if reg.SiteID == "" || reg.HostID == "" {
		http.Error(w, "site_id and host_id required", http.StatusBadRequest)
		return
	}
	if r.Owners == nil {
		http.Error(w, "owner registry not configured", http.StatusInternalServerError)
		return
	}
	r.Owners.Register(reg.SiteID, reg.HostID)
	w.WriteHeader(http.StatusNoContent)
}

// meRoot handles GET /me/<site_id> — the landing page when the operator
// types the bare URL. Forwards to the host's /owner-access/ landing
// page so the host can decide whether to show "log in with passkey" or
// "no devices enrolled yet — go enroll one on LAN first".
func (r *Relay) meRoot(w http.ResponseWriter, req *http.Request) {
	r.meForward(w, req, "/owner-access/")
}

// meTunnel handles /me/<site_id>/{rest...} — generic forwarder.
// Everything past /me/<site_id>/ is passed verbatim through the tunnel
// to the host. The host's auth middleware (cookie check on the
// /owner-access/ family + /api/owner-access/*) decides what's allowed.
func (r *Relay) meTunnel(w http.ResponseWriter, req *http.Request) {
	rest := req.PathValue("rest")
	r.meForward(w, req, "/"+rest)
}

func (r *Relay) meForward(w http.ResponseWriter, req *http.Request, innerPath string) {
	siteID := req.PathValue("site_id")
	if siteID == "" {
		http.Error(w, "missing site_id", http.StatusBadRequest)
		return
	}
	if r.Owners == nil {
		http.Error(w, "owner registry not configured", http.StatusServiceUnavailable)
		return
	}
	hostID, err := r.Owners.Lookup(siteID)
	if err != nil {
		http.Error(w, "site not registered (host may be offline)", http.StatusServiceUnavailable)
		return
	}
	// Preserve the query string so login redirects, ceremony tokens etc.
	// land at the host intact.
	if q := req.URL.RawQuery; q != "" {
		innerPath = innerPath + "?" + q
	}
	body, _ := readBody(req.Body)
	resp, err := r.Queue.Enqueue(req.Context(), hostID, tunnel.TunneledRequest{
		Method: req.Method,
		Path:   innerPath,
		Header: req.Header,
		Body:   body,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}
