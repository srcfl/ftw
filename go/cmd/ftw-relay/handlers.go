package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

const (
	// maxTunnelBodyBytes caps a single tunneled request body so a hostile
	// client can't exhaust relay memory. Dashboards + MCP payloads are well
	// under this.
	maxTunnelBodyBytes = 16 << 20 // 16 MiB
	// enqueueTimeout bounds how long the relay waits for a registered host to
	// answer before giving up with an error. Longer than the poll timeout (so a
	// live host between polls isn't cut off), short enough that a host that died
	// mid-request doesn't pin a goroutine for minutes.
	enqueueTimeout = 60 * time.Second
	// homeStaleAfter is how long after a home Pi's last registration the relay
	// treats it as offline and serves the offline page instead of hanging. The
	// Pi re-registers every 60s, so 2.5× tolerates a missed beat without flapping.
	homeStaleAfter = 150 * time.Second
	// maxRelayBodyBytes is the hard ceiling the limitBody middleware puts on
	// EVERY request body, so no handler — including the JSON control-plane
	// decoders (/tunnel/register, /me/register, /tunnel/.../response, /h/.../approve)
	// — can be made to buffer an unbounded body. Sized above the largest legit
	// payload: a tunneled response carrying a ~16 MiB body base64-expands to ~21 MiB.
	maxRelayBodyBytes = 32 << 20 // 32 MiB
	// maxControlBodyBytes is a much tighter cap for the small, unauthenticated
	// control-plane JSON endpoints (register, me-register, approve). Their
	// payloads are a handful of fields, so anything larger is abuse — reject it
	// before allocating/parsing.
	maxControlBodyBytes = 64 << 10 // 64 KiB
)

var errBodyTooLarge = errors.New("tunneled body exceeds limit")

// ownerHostIDPrefix is the reserved prefix the owner-access Pi derives its
// host_id with (deriveOwnerHostID: "owner-<site>-<rand>"). The owner poll secret
// for such a host_id is minted ONLY by the ES256-verified /me/register, bound to
// the verified site key. The unauthenticated friend path (/tunnel/register) MUST
// refuse any host_id carrying this prefix, so it can never be used to register a
// fake friend session under an owner host_id and be handed the owner's poll
// secret (which then unlocks the /signal rendezvous). Owner and friend host_id
// namespaces are disjoint by this prefix.
const ownerHostIDPrefix = "owner-"

// pairTokenRe bounds the routing token to a safe charset. The host generates
// word-dash tokens (genWordToken: lowercase words joined by '-'); this tolerates
// that plus digits but rejects anything with HTML/JS metacharacters, so the
// token can be rendered into the landing page and so /tunnel/register — which is
// open to the internet — can't be used to plant a token that breaks out of the
// page context.
var pairTokenRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validPairToken(s string) bool {
	return len(s) >= 1 && len(s) <= 80 && pairTokenRe.MatchString(s)
}

// Relay is the in-memory state for one running relay process.
type Relay struct {
	Queue       *tunnel.Queue
	Tokens      *TokenRegistry
	Owners      *OwnerRegistry
	Polls       *PollSecrets
	Signals     *SignalMailbox // blind WebRTC signaling rendezvous (P2P-only home route)
	PollTimeout time.Duration  // 0 → 25s default
	// HomeHost, when set, maps a bare host (e.g. home.fortytwowatts.com) to
	// the single owner Pi registered under HomeSite — forwarding every path
	// verbatim (no /me/<site_id> prefix) so the dashboard's absolute asset
	// paths resolve. The Phase 4 single-home cutover.
	HomeHost string
	HomeSite string
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
	PollSecret  string `json:"poll_secret"`
}

// Handler returns the mux for this relay.
func (r *Relay) Handler() http.Handler {
	if r.Polls == nil {
		r.Polls = NewPollSecrets()
	}
	if r.Signals == nil {
		r.Signals = NewSignalMailbox()
	}
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

	// Blind WebRTC signaling rendezvous (P2P-only home route). The relay parks
	// opaque SDP/signature blobs in a tiny per-site 2-slot mailbox; it never sees
	// plaintext (the resulting DataChannel is DTLS-encrypted end to end).
	//   - browser: POST /signal/{site_id}/offer  + long-poll GET /signal/{site_id}/answer
	//   - Pi:      long-poll GET /signal/{host_id}/offer + POST /signal/{host_id}/answer
	// The Pi side is authenticated with the per-host poll secret (X-FTW-Poll), the
	// same token minted at /me/register; the browser side is rate-limited.
	mux.HandleFunc("POST /signal/{site_id}/offer", r.signalBrowserOffer)
	mux.HandleFunc("GET /signal/{site_id}/answer", r.signalBrowserAnswer)
	mux.HandleFunc("GET /signal/{host_id}/offer", r.signalHostOffer)
	mux.HandleFunc("POST /signal/{host_id}/answer", r.signalHostAnswer)

	// Owner remote access registration (Phase 3). The Pi POSTs its ES256-signed
	// /me/register on startup; the relay pins the key and returns the per-host
	// poll secret the Pi uses BOTH to drain the (friend-flow) tunnel and to
	// authenticate the /signal/* rendezvous. This is control-plane only — there
	// is NO LONGER an owner HTTP request/response tunnel (the /me/<site>/... and
	// /me/<site> forwarders were removed in the P2P-only cutover, slice 6). Owner
	// data exists ONLY as DTLS DataChannel frames now.
	mux.HandleFunc("POST /me/register", r.meRegister)

	// Single-home cutover: a bare host (home.fortytwowatts.com) serves the
	// dashboard's STATIC assets from the owner Pi so the SPA, login page, and
	// p2p.js load at the root with working absolute paths. It is restricted to
	// GET of NON-/api/ paths: the owner API + the ftw_owner cookie never traverse
	// the relay — they ride the DTLS DataChannel only (P2P-only home route). The
	// browser fetches the SPA shell here, then opens the P2P channel for all
	// owner data + the login ceremony.
	if r.HomeHost != "" && r.HomeSite != "" {
		// The browser reaches the relay AS the home host, and Go's ServeMux gives
		// a host-specific pattern precedence over a host-less one — so the
		// host-less /signal/{site}/* browser routes above would be shadowed by the
		// home-host catch-all below. Re-register the BROWSER signaling routes on
		// the home host explicitly so the rendezvous works from the dashboard
		// origin. (The Pi's /signal/{host}/* routes need no host pin: the Pi dials
		// the relay by its own host, not the home host.)
		mux.HandleFunc("POST "+r.HomeHost+"/signal/{site_id}/offer", r.signalBrowserOffer)
		mux.HandleFunc("GET "+r.HomeHost+"/signal/{site_id}/answer", r.signalBrowserAnswer)
		mux.HandleFunc(r.HomeHost+"/", r.homeStaticForward)
	}
	return r.limitBody(mux)
}

// limitBody caps every request body so a hostile client can't exhaust relay
// memory through ANY handler. The per-endpoint readBody discipline only covered
// the tunnel-forward paths; this is the safety net that also bounds the
// unauthenticated JSON control-plane decoders (register/approve/response).
func (r *Relay) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Body != nil {
			req.Body = http.MaxBytesReader(w, req.Body, maxRelayBodyBytes)
		}
		next.ServeHTTP(w, req)
	})
}

// homeStaticForward serves the dashboard's STATIC assets for the bare home host
// from the owner Pi, over the friend-flow tunnel queue, but FAIL-CLOSED for the
// owner data plane: it forwards only GET requests for NON-/api/ paths. The owner
// API and the ftw_owner session cookie therefore NEVER traverse the relay — they
// ride the DTLS DataChannel only (the P2P-only home route). The browser loads
// the SPA shell + login page + p2p.js here, then opens the P2P channel for all
// owner traffic and the login ceremony.
//
// This keeps the app loadable without shipping the web bundle into the relay,
// while structurally guaranteeing (path + method gate) that no cleartext owner
// request or cookie can be tunneled through this host route.
func (r *Relay) homeStaticForward(w http.ResponseWriter, req *http.Request) {
	if r.Owners == nil {
		http.Error(w, "owner registry not configured", http.StatusServiceUnavailable)
		return
	}
	// FAIL-CLOSED owner-data gate. Only GET may traverse the relay for the home
	// host, and never an /api/ path — those carry owner data + the session cookie
	// and must travel inside DTLS only. Anything else is refused here so the
	// relay can never see (or be tricked into proxying) owner traffic.
	if req.Method != http.MethodGet {
		http.Error(w, "owner API is P2P-only; this relay serves static assets only", http.StatusMethodNotAllowed)
		return
	}
	if isOwnerAPIPath(req.URL.Path) {
		http.Error(w, "owner API is P2P-only; not served over the relay", http.StatusForbidden)
		return
	}
	hostID, registered, fresh := r.Owners.Active(r.HomeSite, homeStaleAfter)
	if !registered || !fresh {
		// Never connected, or stopped re-registering: the Pi is offline. Show a
		// reassuring styled page (not a raw timeout/503 that reads as "broken").
		r.serveHomeOffline(w, req, registered)
		return
	}
	// GET assets have no meaningful body; do not read/forward one.
	innerPath := req.URL.Path
	if q := req.URL.RawQuery; q != "" {
		innerPath = innerPath + "?" + q
	}
	// Strip any inbound cookies: the owner session lives only inside DTLS, so a
	// stray ftw_owner on a static-asset GET must never reach the Pi over the relay
	// (it would be ignored, but stripping it keeps the no-owner-cookie-on-relay
	// invariant structural rather than incidental).
	req.Header.Del("Cookie")
	resp, err := r.enqueue(req, hostID, innerPath, nil)
	if err != nil {
		// Fresh a moment ago but the tunnel didn't answer in time — the Pi most
		// likely just dropped. Same reassuring page rather than a bare 502.
		r.serveHomeOffline(w, req, true)
		return
	}
	// Defence in depth: never relay a Set-Cookie from the Pi for the home host —
	// a static asset has no business setting the owner cookie, and the owner
	// session must never appear on a relay-visible response.
	resp.Header.Del("Set-Cookie")
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// isOwnerAPIPath reports whether a request path is part of the owner API surface
// that must travel only inside DTLS — never over the relay. The static home-host
// forwarder refuses these so no owner request/cookie can be tunneled in
// cleartext.
//
// The SOLE exception is GET /api/identity: it returns only the Pi's PUBLIC ES256
// key (no secret, no cookie, no control surface), and the browser MUST fetch +
// pin it BEFORE it can open (and verify) the P2P channel — so it cannot itself
// ride the channel. Allowing this one read is the TOFU bootstrap and costs
// nothing: a key-less relay still cannot forge the signed fingerprint.
func isOwnerAPIPath(p string) bool {
	if p == "/api/identity" {
		return false
	}
	return strings.HasPrefix(p, "/api/")
}

// serveHomeOffline renders a calm, self-contained "home offline" page for the
// home.* route when the Pi isn't reachable. It is self-contained (inline CSS,
// no external assets) because the relay can't fetch the Pi's stylesheet while
// the Pi is offline. registered distinguishes "never connected" from "was
// connected, dropped" so the copy is honest. A gentle JS auto-retry reloads the
// page so it recovers on its own once the Pi comes back.
func (r *Relay) serveHomeOffline(w http.ResponseWriter, _ *http.Request, registered bool) {
	headline := "We haven’t reached your home yet"
	detail := "This home hasn’t checked in with the relay. If you just set it up, give it a minute — it connects automatically."
	if registered {
		detail = "It was connected recently but isn’t responding right now — it may be powered off or without internet. Your data stays on the device; nothing is lost."
		headline = "Your home is offline right now"
	}
	page := strings.NewReplacer(
		"{{HEADLINE}}", esc(headline),
		"{{DETAIL}}", esc(detail),
	).Replace(offlineHTML)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, page)
}

// offlineHTML is the self-contained offline page. Brand-aligned (single amber
// accent, near-black on-accent text, hairline borders, mono eyebrow) without
// depending on theme.css, which lives on the offline Pi.
const offlineHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>forty-two-watts — home offline</title>
<style>
  :root { --accent: #f0c040; --ink: #1a1a1a; --muted: #6b6b6b; --line: #e6e3dd; --bg: #faf9f7; }
  * { box-sizing: border-box; }
  body { font-family: system-ui, -apple-system, sans-serif; background: var(--bg); color: var(--ink);
         margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 1.5rem; }
  .card { width: 100%; max-width: 30rem; background: #fff; border: 1px solid var(--line);
          border-radius: .75rem; padding: 2rem; }
  .eyebrow { font-family: ui-monospace, "SF Mono", Menlo, monospace; text-transform: uppercase;
             letter-spacing: .18em; font-size: .7rem; color: var(--muted); display: flex; align-items: center; gap: .5rem; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--accent);
         box-shadow: 0 0 0 0 rgba(240,192,64,.6); animation: pulse 2s infinite; }
  @keyframes pulse { 0% { box-shadow: 0 0 0 0 rgba(240,192,64,.5); } 70% { box-shadow: 0 0 0 .6rem rgba(240,192,64,0); } 100% { box-shadow: 0 0 0 0 rgba(240,192,64,0); } }
  h1 { font-size: 1.4rem; margin: 1.25rem 0 .5rem; line-height: 1.25; }
  p { color: var(--muted); line-height: 1.55; margin: 0 0 1.5rem; }
  button { font: inherit; font-weight: 600; background: var(--accent); color: #0a0a0a; border: 0;
           border-radius: .4rem; padding: .6rem 1.25rem; cursor: pointer; }
  .retry { margin-top: 1rem; font-size: .8rem; color: var(--muted); }
</style>
</head><body>
  <main class="card">
    <div class="eyebrow"><span class="dot"></span> forty-two-watts</div>
    <h1>{{HEADLINE}}</h1>
    <p>{{DETAIL}}</p>
    <button id="retry" type="button">Try again</button>
    <div class="retry">Checking again in <span id="count">20</span>s…</div>
  </main>
<script>
  document.getElementById('retry').addEventListener('click', () => location.reload());
  let n = 20;
  const el = document.getElementById('count');
  setInterval(() => { n -= 1; if (n <= 0) { location.reload(); return; } el.textContent = n; }, 1000);
</script>
</body></html>`

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
	// Authenticate the poller against the token minted at registration, so a
	// caller that only learned host_id can't poll for (and steal) this host's
	// tunneled traffic. Unknown host / wrong token → 401. The host retries.
	if r.Polls == nil || !r.Polls.Check(hostID, req.Header.Get(tunnel.PollSecretHeader)) {
		http.Error(w, "unauthorized poll", http.StatusUnauthorized)
		return
	}
	tr, err := r.Queue.Poll(req.Context(), hostID, r.pollTimeout())
	if errors.Is(err, tunnel.ErrPollTimeout) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if errors.Is(err, tunnel.ErrTooManyWaiters) {
		http.Error(w, "relay at capacity", http.StatusServiceUnavailable)
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
	hostID := req.PathValue("host_id")
	// Same poll-token auth as tunnelNext: only the real host may post responses,
	// so a caller knowing only host_id (+ a guessed req_id) can't inject one.
	if r.Polls == nil || !r.Polls.Check(hostID, req.Header.Get(tunnel.PollSecretHeader)) {
		http.Error(w, "unauthorized poll", http.StatusUnauthorized)
		return
	}
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
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
	var reg registerRequest
	if err := json.NewDecoder(req.Body).Decode(&reg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// /tunnel/register is open to the internet. Reject any token that isn't a
	// plain routing key so it can never carry HTML/JS into the landing page.
	if !validPairToken(reg.Token) {
		http.Error(w, "invalid token", http.StatusBadRequest)
		return
	}
	// CRITICAL: refuse owner-namespaced host_ids on the unauthenticated friend
	// path. The owner poll secret for an owner-<…> host_id is minted only by the
	// ES256-verified /me/register; if a friend could register one here it would be
	// handed back that secret (Issue's principal binding also blocks it, but
	// rejecting outright keeps owner/friend host_id namespaces structurally
	// disjoint and avoids ever touching the owner secret store from this path).
	if strings.HasPrefix(reg.HostID, ownerHostIDPrefix) {
		http.Error(w, "host_id namespace reserved", http.StatusForbidden)
		return
	}
	if reg.TTLMs <= 0 {
		http.Error(w, "ttl_ms must be positive", http.StatusBadRequest)
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
	if errors.Is(err, ErrTooManyTokens) {
		http.Error(w, "relay at capacity", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	secret := ""
	if r.Polls != nil {
		// Bind the friend's poll secret to the pair token that minted it (the
		// principal). A subsequent register for the same host_id with a different
		// token can't retrieve this secret, and the owner path (principal = site
		// key) can never collide with it.
		s, err := r.Polls.Issue(reg.HostID, "pair:"+reg.Token)
		if err != nil {
			// The host_id already has a secret minted by a different principal.
			// Don't disclose it: refuse the registration.
			http.Error(w, "host_id in use by another principal", http.StatusForbidden)
			return
		}
		secret = s
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(registerResponse{
		PublicURL:   fmt.Sprintf("/h/%s", reg.Token),
		ApprovalURL: fmt.Sprintf("/h/%s/approve", reg.Token),
		PollSecret:  secret,
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
	//   4. "const TOKEN = %s" — the actual session token, used by the JS
	//      to POST to /h/<token>/approve. Getting this wrong is fatal:
	//      the approve POST hits the wrong path and the relay returns
	//      403, which the JS surfaces as "Wrong code" even when the
	//      friend typed the correct 4-digit code. It is emitted via
	//      json.Marshal (not %q), which \u-escapes <, > and & so a token
	//      can never break out of the <script> context — defence in depth
	//      behind validPairToken on /tunnel/register.
	tokJSON, _ := json.Marshal(tok)
	fmt.Fprintf(w, landingHTML, esc(t.As()), esc(t.Intent()), t.State(), tokJSON)
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
const TOKEN = %s;
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
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
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
	body, err := readBody(req.Body)
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	resp, err := r.enqueue(req, t.HostID(), innerPath, body)
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
	return readBodyLimited(r, maxTunnelBodyBytes)
}

// readBodyLimited reads up to max bytes, returning errBodyTooLarge if the source
// has more (rather than silently truncating). Split out so the cap is testable
// without allocating a 16 MiB fixture.
func readBodyLimited(r io.Reader, max int64) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	// Read one byte past the limit so we can tell "exactly at the cap" from
	// "over it".
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errBodyTooLarge
	}
	return b, nil
}

// enqueue forwards one request through the tunnel to hostID, bounded by
// enqueueTimeout so a registered-but-dead host fails fast instead of pinning
// the goroutine until the client gives up.
func (r *Relay) enqueue(req *http.Request, hostID, innerPath string, body []byte) (tunnel.TunneledResponse, error) {
	ctx, cancel := context.WithTimeout(req.Context(), enqueueTimeout)
	defer cancel()
	return r.Queue.Enqueue(ctx, hostID, tunnel.TunneledRequest{
		Method: req.Method,
		Path:   innerPath,
		Header: req.Header,
		Body:   body,
	})
}

// ---- Owner remote access (Phase 3) ----

type meRegisterRequest struct {
	SiteID string `json:"site_id"`
	HostID string `json:"host_id"`
	// PublicKey is the owner Pi's ES256 identity key, hex X||Y (128 chars).
	// TsMs is the registration time (ms since epoch). Sig is the raw R||S
	// ES256 signature (hex) over MeRegisterSigningString(site,host,ts). These
	// authenticate the registration so only the holder of the site's private
	// key can set or change its host_id mapping.
	PublicKey string `json:"public_key"`
	TsMs      int64  `json:"ts_ms"`
	Sig       string `json:"sig"`
}

func (r *Relay) meRegister(w http.ResponseWriter, req *http.Request) {
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
	var reg meRegisterRequest
	if err := json.NewDecoder(req.Body).Decode(&reg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if reg.SiteID == "" || reg.HostID == "" || reg.PublicKey == "" || reg.Sig == "" {
		http.Error(w, "site_id, host_id, public_key and sig required", http.StatusBadRequest)
		return
	}
	if r.Owners == nil {
		http.Error(w, "owner registry not configured", http.StatusInternalServerError)
		return
	}
	// Reject stale/future registrations so a captured body cannot be replayed
	// indefinitely. now() is read once; the skew window is symmetric.
	now := time.Now().UnixMilli()
	if d := now - reg.TsMs; d > tunnel.MeRegisterMaxSkewMs || d < -tunnel.MeRegisterMaxSkewMs {
		http.Error(w, "registration timestamp out of range", http.StatusUnauthorized)
		return
	}
	// Verify the signature binds (site_id, host_id, ts) to the presented key.
	msg := tunnel.MeRegisterSigningString(reg.SiteID, reg.HostID, reg.TsMs)
	if !verifyES256Hex(reg.PublicKey, msg, reg.Sig) {
		http.Error(w, "invalid registration signature", http.StatusUnauthorized)
		return
	}
	// Enforce key continuity: the site is pinned to the first key it saw (or to
	// the operator-provisioned home key). A different key is refused so nobody
	// can hijack an existing site's mapping.
	if err := r.Owners.Register(reg.SiteID, reg.HostID, reg.PublicKey); err != nil {
		if errors.Is(err, ErrTooManyOwners) {
			http.Error(w, "relay at capacity", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	// Return the poll token this host must present on /tunnel/<host>/next and the
	// /signal/* rendezvous. The ES256-verified registration above proves it owns
	// host_id, so only it learns the token. The secret is bound to the verified
	// site PUBLIC KEY as its principal, so the unauthenticated friend path (which
	// can only present a pair-token principal) can never retrieve it even if it
	// learned the owner host_id. Re-registration with the same key returns the
	// same token (refreshes after a relay restart re-mints).
	secret := ""
	if r.Polls != nil {
		s, err := r.Polls.Issue(reg.HostID, "site:"+reg.PublicKey)
		if err != nil {
			// The host_id's secret is bound to a different principal — refuse
			// rather than disclose it. (Should not happen for a key-continuous
			// site; defends against a host_id reused across principals.)
			http.Error(w, "poll secret bound to a different principal", http.StatusForbidden)
			return
		}
		secret = s
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"poll_secret": secret})
}

// ---- Blind WebRTC signaling rendezvous (P2P-only home route) ----

// signalBrowserOffer parks a browser's SDP offer for {site_id} and wakes any Pi
// offer-poller. Unauthenticated (the channel that results is itself
// authenticated end to end by the signed-fingerprint handshake + owner login
// over the DataChannel), but rate-limited and body-capped so it can't be abused
// to spin the mailbox.
func (r *Relay) signalBrowserOffer(w http.ResponseWriter, req *http.Request) {
	siteID := req.PathValue("site_id")
	if siteID == "" {
		http.Error(w, "missing site_id", http.StatusBadRequest)
		return
	}
	if r.Signals == nil {
		http.Error(w, "signaling not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := readBodyLimited(req.Body, maxSignalOfferBytes)
	if err != nil {
		http.Error(w, "offer too large", http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty offer", http.StatusBadRequest)
		return
	}
	if err := r.Signals.ParkOffer(siteID, body); err != nil {
		if err == errSignalRateLimited {
			http.Error(w, "too many offers", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "signaling at capacity", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// signalBrowserAnswer long-polls for the Pi's parked answer for {site_id}. The
// browser calls this after POSTing its offer; it returns the opaque answer blob
// (SDP + fp_sig + ts) verbatim, or 204 to re-poll.
func (r *Relay) signalBrowserAnswer(w http.ResponseWriter, req *http.Request) {
	siteID := req.PathValue("site_id")
	if siteID == "" {
		http.Error(w, "missing site_id", http.StatusBadRequest)
		return
	}
	if r.Signals == nil {
		http.Error(w, "signaling not configured", http.StatusServiceUnavailable)
		return
	}
	data, ok := r.Signals.TakeAnswer(siteID, signalPollTimeout)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// signalHostOffer is the Pi's long-poll for a parked offer. AUTHENTICATED with
// the per-host poll secret (the same token minted at /me/register and used on
// /tunnel/{host}/next), then host_id is mapped back to its site_id via the
// ES256-pinned OwnerRegistry so a Pi only ever drains offers for its own site.
func (r *Relay) signalHostOffer(w http.ResponseWriter, req *http.Request) {
	hostID := req.PathValue("host_id")
	if r.Polls == nil || !r.Polls.Check(hostID, req.Header.Get(tunnel.PollSecretHeader)) {
		http.Error(w, "unauthorized poll", http.StatusUnauthorized)
		return
	}
	if r.Owners == nil || r.Signals == nil {
		http.Error(w, "signaling not configured", http.StatusServiceUnavailable)
		return
	}
	siteID, err := r.Owners.SiteForHost(hostID)
	if err != nil {
		http.Error(w, "host not registered", http.StatusServiceUnavailable)
		return
	}
	data, ok := r.Signals.TakeOffer(siteID, signalPollTimeout)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// signalHostAnswer parks the Pi's answer blob (SDP + fp_sig + ts) for delivery
// to the browser, waking the browser's answer-poller. AUTHENTICATED with the
// per-host poll secret, then host_id->site_id via the registry.
func (r *Relay) signalHostAnswer(w http.ResponseWriter, req *http.Request) {
	hostID := req.PathValue("host_id")
	if r.Polls == nil || !r.Polls.Check(hostID, req.Header.Get(tunnel.PollSecretHeader)) {
		http.Error(w, "unauthorized poll", http.StatusUnauthorized)
		return
	}
	if r.Owners == nil || r.Signals == nil {
		http.Error(w, "signaling not configured", http.StatusServiceUnavailable)
		return
	}
	siteID, err := r.Owners.SiteForHost(hostID)
	if err != nil {
		http.Error(w, "host not registered", http.StatusServiceUnavailable)
		return
	}
	body, err := readBodyLimited(req.Body, maxSignalAnswerBytes)
	if err != nil {
		http.Error(w, "answer too large", http.StatusRequestEntityTooLarge)
		return
	}
	if len(body) == 0 {
		http.Error(w, "empty answer", http.StatusBadRequest)
		return
	}
	r.Signals.ParkAnswer(siteID, body)
	w.WriteHeader(http.StatusNoContent)
}
