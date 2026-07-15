package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/tunnel"
)

// relaySigner is the ES256 identity used to authenticate owner-access relay
// registrations. Satisfied by *nova.Identity (the self-sovereign site key
// loaded in main, independent of whether Nova federation is enabled).
type relaySigner interface {
	PublicKeyHex() string
	SignRawHex(msg string) (string, error)
}

// trustedDevicePubkeysLoader, when set, returns the current set of trusted
// device_pubkeys (C1) to publish on every /me/register. It is a package-level
// hook rather than a parameter so the single call site in main.go does not need
// to change as this area lands in parallel — main.go wires it once, right after
// opening the state store, with:
//
//	trustedDevicePubkeysLoader = func() []string {
//	    pks, err := st.TrustedDevicePubkeys()
//	    if err != nil { slog.Warn("owner-access: load device pubkeys", "err", err); return nil }
//	    return pks
//	}
//
// Nil (unwired) means the "device_pubkeys" field is OMITTED from the body — the
// relay then publishes no device-key set for the site and its device-gate (C2)
// stays closed, which is the correct fail-closed default before wiring.
var trustedDevicePubkeysLoader func() []string

const ownerSiteIDKey = "owner_site_id"

// loadTrustedDevicePubkeys returns the device-key set to publish, or nil when no
// loader is wired or it errors. Always returns a non-nil-safe slice the caller
// only marshals when len>0, so the field is omitted rather than sent as [].
func loadTrustedDevicePubkeys() []string {
	if trustedDevicePubkeysLoader == nil {
		return nil
	}
	return trustedDevicePubkeysLoader()
}

// deriveOwnerSiteID returns the stable, relay-routed site_id for owner remote
// access. It is opaque and high-entropy so two default "Home" Pis cannot collide
// on the public relay and the relay never routes by a human-readable label.
func deriveOwnerSiteID(st *state.Store) string {
	if st != nil {
		if existing, ok := st.LoadConfig(ownerSiteIDKey); ok && existing != "" {
			if isOpaqueOwnerSiteID(existing) {
				return existing
			}
		}
	}

	b := make([]byte, 24)
	_, _ = randomRead(b)
	siteID := "site:" + base64.RawURLEncoding.EncodeToString(b)
	if st != nil {
		_ = st.SaveConfig(ownerSiteIDKey, siteID)
	}
	return siteID
}

func isOpaqueOwnerSiteID(siteID string) bool {
	const prefix = "site:"
	if !strings.HasPrefix(siteID, prefix) {
		return false
	}
	token := strings.TrimPrefix(siteID, prefix)
	if len(token) < 32 {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// deriveOwnerHostID returns a stable host_id for the owner-access
// relay registration. It looks up (or creates) a row in the
// state.db config table so the host_id survives restarts — important
// because the relay's site_id → host_id mapping is in-memory and any
// long-poll already in flight on the previous host_id is stranded.
func deriveOwnerHostID(st *state.Store, siteName string) string {
	const key = "owner_relay_host_id"
	if existing, ok := st.LoadConfig(key); ok && existing != "" {
		return existing
	}
	// Generate a stable random suffix; site name alone leaks identity
	// and isn't unique across multiple instances at the same site.
	b := make([]byte, 6)
	_, _ = randomRead(b)
	hostID := "owner-" + sanitizeSiteName(siteName) + "-" + hex.EncodeToString(b)
	_ = st.SaveConfig(key, hostID)
	return hostID
}

// randomRead is a tiny shim so tests can stub crypto/rand. main.go
// already has its own RNG paths; this is the one place we need a few
// bytes for an identifier so we don't pull in the full set of helpers.
func randomRead(b []byte) (int, error) {
	return cryptoRandRead(b)
}

// newTunnelMarker returns a 256-bit random hex secret used to mark
// relay-tunnelled requests (see api.Deps.TunnelMarker). Generated once per
// process; never persisted, never leaves the host.
func newTunnelMarker() string {
	b := make([]byte, 32)
	_, _ = randomRead(b)
	return hex.EncodeToString(b)
}

func sanitizeSiteName(s string) string {
	if s == "" {
		return "site"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+('a'-'A'))
		case c == ' ' || c == '_':
			out = append(out, '-')
		}
	}
	if len(out) == 0 {
		return "site"
	}
	return string(out)
}

// runOwnerRelayRegistration is a long-lived goroutine that registers
// this host with the relay every 60s. Idempotent on the relay side
// (Register just upserts). The host also runs the long-poll loop
// against the relay via the owner-access tunnel (next commit wires
// that up); the registration step here is purely "tell the relay
// which host_id to enqueue /me/<site>/* requests onto".
//
// Also starts the long-poll loop that drains those requests and
// forwards them to the local API server.
func runOwnerRelayRegistration(ctx context.Context, relayURL, siteID, hostID, tunnelMarker string, apiPort int, signer relaySigner, p2p p2pAnswerer) {
	relayURL = strings.TrimRight(relayURL, "/")
	// The public key is not a secret; log it in full so an operator can pin it
	// on the relay with -home-pubkey (closes the post-restart TOFU race).
	slog.Info("owner-access: relay registration identity",
		"site_id", siteID, "public_key", signer.PublicKeyHex())

	// The owner HTTP request/response tunnel is GONE (P2P-only cutover, slice 6):
	// owner data exists only as DTLS DataChannel frames. The relay registration
	// below pins the site key and returns the per-host poll secret used BOTH for
	// the signaling loop AND the static-asset poller below.
	polls := &pollSecretHolder{}

	// STATIC-ONLY asset host. The relay still needs to serve the SPA shell, the
	// login page, p2p.js, and the /api/identity TOFU anchor — these have no
	// owner data. We drain /tunnel/{host}/next and reverse-proxy to the local API,
	// but FAIL-CLOSED: only GET of non-/api/ paths (plus GET /api/identity) is
	// served; the owner API + cookie are refused here too, as defence in depth
	// behind the relay's identical refusal. No owner request or session ever
	// rides this transport.
	// MULTI-TENANT enroll-forward: the relay's bootstrapEnrollForward enqueues
	// POST /api/owner-access/enroll/{start,finish} onto THIS host's tunnel queue
	// during the first-enrollment window (the new owner has no device key yet, so
	// they can't open the P2P channel to enroll one — this is the one HTTP bridge
	// across that gap). The host below additionally accepts those two POSTs and
	// stamps X-FTW-Tunnel so the Pi's isTunneled gate fires (PIN + possession-proof
	// + zero-device recheck + owner-cookie suppression). Enabled by default for
	// the official home.fortytwowatts.com flow; remote access still remains opt-in
	// in the Pi config, and FTW_MULTI_TENANT=off is a hardening escape hatch for a
	// self-hosted relay that never serves first-device bootstrap.
	multiTenant := ownerRelayMultiTenantEnabled()
	staticHost := buildStaticAssetHost(relayURL, hostID, apiPort, multiTenant, tunnelMarker)
	go staticHost.Run(ctx)

	// The blind WebRTC signaling loop is the ONLY owner-DATA transport: it
	// long-polls browser offers, answers them over P2P with fail-closed replay
	// headers (remote marker stamped, no cookie), and parks the signed answer.
	// Skipped only when no P2P manager is wired (identity load failed), in which
	// case there is no owner remote access at all.
	if p2p != nil {
		go runOwnerSignalLoop(ctx, relayURL, hostID, tunnelMarker, p2p, polls)
	} else {
		slog.Warn("owner-access: no P2P manager wired — remote access disabled (no owner transport)")
	}

	registerOnce := func() {
		tsMs := time.Now().UnixMilli()
		// C1: publish the set of trusted device_pubkeys so the relay can gate
		// signaling offers (C2) on a browser proving one of them. When any exist we
		// sign the v2 string that COVERS the set, so a captured register body can't
		// be replayed with a swapped/added key to inject one the relay would trust
		// (the v1 signature did not cover the array — that was the bug). With no
		// device keys yet we sign v1 and omit the field; the relay's device-gate
		// then has nothing to admit and stays closed (fail-closed default).
		pks := loadTrustedDevicePubkeys()
		signString := tunnel.MeRegisterSigningString(siteID, hostID, tsMs)
		if len(pks) > 0 {
			signString = tunnel.MeRegisterSigningStringV2(siteID, hostID, tsMs, pks)
		}
		sig, err := signer.SignRawHex(signString)
		if err != nil {
			slog.Warn("owner-access: sign register", "err", err)
			return
		}
		reqBody := map[string]any{
			"site_id":    siteID,
			"host_id":    hostID,
			"public_key": signer.PublicKeyHex(),
			"ts_ms":      tsMs,
			"sig":        sig,
		}
		if len(pks) > 0 {
			reqBody["device_pubkeys"] = pks
		}
		body, _ := json.Marshal(reqBody)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/me/register", bytes.NewReader(body))
		if err != nil {
			slog.Warn("owner-access: build register request", "err", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			slog.Warn("owner-access: register with relay", "err", err, "relay", relayURL)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			slog.Warn("owner-access: relay rejected register", "status", resp.StatusCode)
			return
		}
		var out struct {
			PollSecret string `json:"poll_secret"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			slog.Warn("owner-access: decode register response", "err", err)
			return
		}
		// Update the poll token (a relay restart re-mints it, so refresh on every
		// registration). Both the signaling loop and the static-asset host present
		// it on their relay polls.
		polls.set(out.PollSecret)
		staticHost.SetPollSecret(out.PollSecret)
		slog.Info("owner-access: registered with relay", "site_id", siteID, "host_id", hostID)
	}

	// Register immediately, then every 60s. A relay restart drops all
	// registrations, so we re-register periodically to recover (and refresh the
	// poll token) without host intervention.
	registerOnce()
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			registerOnce()
		}
	}
}

// pollSecretHolder carries the relay-minted per-host poll secret, refreshed on
// every registration (a relay restart re-mints it). It satisfies the signaling
// loop's pollSecretSource so that loop always presents the current token. Safe
// for concurrent use: the registration loop writes, the signaling loop reads.
type pollSecretHolder struct {
	mu     sync.Mutex
	secret string
}

func (p *pollSecretHolder) set(s string) {
	p.mu.Lock()
	p.secret = s
	p.mu.Unlock()
}

// PollSecret implements pollSecretSource.
func (p *pollSecretHolder) PollSecret() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.secret
}

// buildStaticAssetHost constructs (but does not run) the host-side long-poll for
// serving the dashboard's STATIC assets over the relay's friend-flow tunnel
// queue. It reverse-proxies forwarded requests to the local API, but ONLY for
// GET of non-/api/ paths plus GET /api/identity — the owner API + session cookie
// are refused here (404/no-cookie) as defence in depth behind the relay's
// identical refusal. So no owner request or session ever rides this transport;
// owner data exists solely as DTLS DataChannel frames.
//
// Unlike the removed owner tunnel, the STATIC paths do NOT stamp the X-FTW-Tunnel
// marker: they only ever proxy public assets, which the local gate already serves
// without auth, and never an owner API call. The two MULTI-TENANT enroll routes
// (when enabled) are the sole exception — they DO stamp the marker, see
// staticAssetHandler.
func buildStaticAssetHost(relayURL, hostID string, apiPort int, multiTenant bool, tunnelMarker string) *tunnel.Host {
	return tunnel.NewHost(relayURL, hostID, buildStaticAssetHandler(apiPort, multiTenant, tunnelMarker))
}

func ownerRelayMultiTenantEnabled() bool {
	return envBoolDefault("FTW_MULTI_TENANT", true)
}

// buildStaticAssetHandler constructs the fail-closed tunnel handler. It is split
// out from buildStaticAssetHost so it can be exercised directly against a fake
// local API in tests (the reverse proxy targets 127.0.0.1:<apiPort>).
func buildStaticAssetHandler(apiPort int, multiTenant bool, tunnelMarker string) http.Handler {
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", apiPort))
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, fmt.Sprintf("local api unavailable: %v", err), http.StatusBadGateway)
	}
	return &staticAssetHandler{proxy: proxy, multiTenant: multiTenant, tunnelMarker: tunnelMarker}
}

// enrollForwardPaths are the ONLY owner-API paths this host forwards to the Pi,
// and only under multi-tenant. They mirror the relay's bootstrapEnrollForward
// allowlist — the first-enrollment bridge for an owner who has no device key yet.
var enrollForwardPaths = map[string]struct{}{
	"/api/owner-access/enroll/start":  {},
	"/api/owner-access/enroll/finish": {},
}

// staticAssetHandler is the fail-closed wrapper around the reverse proxy: it
// serves only public static assets (and the /api/identity TOFU anchor), refusing
// every other /api/ path and every non-GET method so the owner API can never be
// reached over the relay tunnel even if the relay's own refusal were bypassed.
//
// Under multiTenant it grows ONE narrow exception: POST of the two enroll routes
// is forwarded to the local API with X-FTW-Tunnel stamped (so the Pi's isTunneled
// gate fires — PIN + possession-proof + zero-device recheck + owner-cookie
// suppression). No other path or method is broadened.
type staticAssetHandler struct {
	proxy        *httputil.ReverseProxy
	multiTenant  bool
	tunnelMarker string
}

func (h *staticAssetHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// MULTI-TENANT enroll-forward: the relay enqueues POST enroll/{start,finish}
	// onto this host's queue during the first-enrollment window. Stamp the trusted
	// per-process marker (overwriting whatever rode in — a relay/browser can't
	// forge it) so s.isTunneled(r) is true Pi-side, then forward to the local API.
	// Set-Cookie is stripped on the way back exactly like the static path. ONLY
	// these two exact paths + POST; everything else falls through to the
	// fail-closed gate below (so a non-enroll /api POST is still 403/405).
	if h.multiTenant && r.Method == http.MethodPost {
		if _, ok := enrollForwardPaths[r.URL.Path]; ok {
			r.Header.Set("X-FTW-Tunnel", h.tunnelMarker)
			rec := &stripSetCookieWriter{ResponseWriter: w}
			h.proxy.ServeHTTP(rec, r)
			return
		}
	}
	if r.Method != http.MethodGet {
		writeStaticHostP2POnly(w, r, http.StatusMethodNotAllowed, "FTW_REMOTE_P2P_ONLY",
			"owner API is P2P-only; this Pi tunnel serves static assets and first-device setup only")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/identity" {
		writeStaticHostP2POnly(w, r, http.StatusForbidden, "FTW_REMOTE_API_P2P_ONLY",
			"owner API is P2P-only; wait for the secure channel")
		return
	}
	// Strip any owner cookie before proxying to the local API: a static asset
	// must never carry the owner session, and the local API must never set one on
	// this path. Belt-and-braces behind the relay's own cookie stripping. (The
	// enroll-forward path above does NOT strip the request Cookie — the PIN/proof
	// ride the query string, and the Pi suppresses the response cookie itself.)
	r.Header.Del("Cookie")
	rec := &stripSetCookieWriter{ResponseWriter: w}
	h.proxy.ServeHTTP(rec, r)
}

func writeStaticHostP2POnly(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	w.Header().Set("X-FTW-Error-Code", code)
	slog.Warn("owner-access tunnel request rejected",
		"code", code,
		"status", status,
		"method", r.Method,
		"path", r.URL.Path,
		"remote", r.RemoteAddr)
	http.Error(w, code+": "+msg, status)
}

// stripSetCookieWriter drops any Set-Cookie the local API emits on the static
// path, so the owner session can never leave the Pi over the relay tunnel.
type stripSetCookieWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *stripSetCookieWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.Header().Del("Set-Cookie")
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *stripSetCookieWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.Header().Del("Set-Cookie")
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}
