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
	// surfaces "friend opened the URL — call you yet?".
	r.Tokens.MarkPendingHit(tok)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, landingHTML, esc(t.As()), esc(t.Intent()), t.ApprovalCode(), t.State())
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

const landingHTML = `<!doctype html>
<html><head><title>forty-two-watts pair session</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; padding: 0 1rem; color: #222; }
  code { background: #f4f4f4; padding: .2rem .4rem; border-radius: .2rem; }
  .code { font-size: 3rem; letter-spacing: .3em; text-align: center; padding: 2rem;
          background: #fffbea; border: 1px solid #f0c040; border-radius: .5rem; margin: 2rem 0; }
</style>
</head><body>
<h1>Connect to a forty-two-watts instance</h1>
<p>Identity: <code>%s</code></p>
<p>Intent: %s</p>
<p>Tell the host this code over voice (phone, Signal call, etc.):</p>
<div class="code">%s</div>
<p>State: <code>%s</code>. This page does not refresh automatically.</p>
</body></html>`

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
