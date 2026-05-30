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

// Handler returns the mux for this relay.
func (r *Relay) Handler() http.Handler {
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

func (r *Relay) publicLanding(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	t, err := r.Tokens.Get(tok)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	// Bump the pending-approval counter so the operator's dashboard
	// surfaces "friend opened the URL — waiting for them to enter the code".
	r.Tokens.MarkPendingHit(tok)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Format args (in landingHTML positional order):
	//   1. "From: %s"        — claimed identity (host-supplied "as")
	//   2. "Intent: %s"      — what the host says the session is for
	//   3. "State: %s"       — current token state
	//   4. "const TOKEN = %q" — the actual session token, used by the JS
	//      to POST to /h/<token>/approve. Getting this wrong is fatal:
	//      the approve POST hits the wrong path and the relay returns
	//      403, which the JS surfaces as "Wrong code" even when the
	//      friend typed the correct 4-digit code.
	fmt.Fprintf(w, landingHTML, esc(t.As()), esc(t.Intent()), t.State(), tok)
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
// here to activate the session. Same-origin POST to /approve so there
// are no CORS surprises.
//
// Format args (in order): token, claimed identity (host-supplied "as"),
// intent, current token state.
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
const TOKEN = %q;
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
    const resp = await fetch('/h/' + encodeURIComponent(TOKEN) + '/approve', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ code }),
    });
    if (resp.ok) {
      const data = await resp.json();
      const grant = (data && data.grant) ? data.grant : '';
      const mcpURL = location.origin + '/h/' + encodeURIComponent(TOKEN) + '/mcp';
      const addCmd = 'claude mcp add ftw-friend --transport http ' + mcpURL +
        ' --header "Authorization: Bearer ' + grant + '"';
      msg.className = 'ok';
      msg.innerHTML = 'Activated. Add this to Claude Code:<br><br>' +
        '<code>' + addCmd.replace(/&/g, '&amp;').replace(/</g, '&lt;') + '</code>' +
        '<p class="muted">The Bearer token is your session secret — anyone with it can use the tools. ' +
        'It is shown only once, here, and expires when the session does.</p>';
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

// grantCookie is the cookie name carrying the session grant for browser
// (dashboard) access. Path-scoped to the token so it is only sent on that
// session's routes.
const grantCookie = "ftw_grant"

func (r *Relay) publicApprove(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
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
	// Approval just minted the session grant. Hand it to the friend two
	// ways: an HttpOnly cookie (browser → dashboard) and the JSON body
	// (so the landing-page JS can print the MCP `--header "Authorization:
	// Bearer …"` one-liner). The 4-digit code's whole job is this one-time
	// exchange; from here the grant — not the URL — is the access secret.
	t, err := r.Tokens.Get(tok)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	grant := t.Grant()
	http.SetCookie(w, &http.Cookie{
		Name:     grantCookie,
		Value:    grant,
		Path:     "/h/" + tok + "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(time.Until(t.ExpiresAt()).Seconds()),
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"grant": grant})
}

// bearerGrant pulls the grant from an "Authorization: Bearer <grant>"
// header (the MCP client path).
func bearerGrant(req *http.Request) string {
	const p = "Bearer "
	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, p) {
		return ""
	}
	return strings.TrimPrefix(auth, p)
}

func (r *Relay) publicMCP(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	if !r.Tokens.CheckGrant(tok, bearerGrant(req)) {
		http.Error(w, "missing or invalid session grant", http.StatusUnauthorized)
		return
	}
	// Never forward the relay-side session secret to the host.
	req.Header.Del("Authorization")
	r.tunnelPublic(w, req, tok, "/mcp")
}

func (r *Relay) publicWeb(w http.ResponseWriter, req *http.Request) {
	tok := req.PathValue("token")
	c, _ := req.Cookie(grantCookie)
	grant := ""
	if c != nil {
		grant = c.Value
	}
	if !r.Tokens.CheckGrant(tok, grant) {
		http.Error(w, "missing or invalid session grant", http.StatusUnauthorized)
		return
	}
	// Strip cookies before tunneling — the friend's grant cookie is a
	// relay secret and the host dashboard has no use for friend cookies.
	req.Header.Del("Cookie")
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
