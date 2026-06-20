package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
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

// homeRouteSiteCookieName is a browser-local routing hint written by the public
// remote loader after it decrypts the owner's directory. It contains only the
// opaque site_id. With it present, the relay can forward STATIC app GETs to the
// chosen Pi without storing or shipping the app bundle itself. It is deliberately
// not an auth signal: /api/* remains refused at the relay and owner data still
// rides P2P.
const homeRouteSiteCookieName = "ftw_home_site"

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
	Queue      *tunnel.Queue
	Tokens     *TokenRegistry
	Owners     *OwnerRegistry
	Polls      *PollSecrets
	Signals    *SignalMailbox    // blind WebRTC signaling rendezvous (P2P-only home route)
	Challenges *SignalChallenges // single-use device-key proof nonces for the signaling offer (C2)
	OfferLimit *IPRateLimiter    // per-source-IP throttle on browser signaling offers (FIX-C)
	ICELimit   *IPRateLimiter    // SEPARATE per-source-IP throttle on GET /signal/ice so an ICE
	// fetch never spends an offer token (avoids halving the offer burst)
	TrustCFIP   bool          // honour CF-Connecting-IP from validated Cloudflare peers (-trust-cf-ip)
	PollTimeout time.Duration // 0 → 25s default
	// HomeHost, when set, maps a bare host (e.g. home.fortytwowatts.com) to
	// the single owner Pi registered under HomeSite — forwarding every path
	// verbatim (no /me/<site_id> prefix) so the dashboard's absolute asset
	// paths resolve. The Phase 4 single-home cutover.
	HomeHost string
	HomeSite string
	// HomeWeb is the relay VM's tiny bootstrap bundle for the public home host.
	// In multi-tenant mode it is NOT the app bundle: only the landing/login/loader
	// allowlist is served from disk. Once the browser decrypts its directory, a
	// routing cookie tells the relay which registered Pi should receive static app
	// GETs. Owner /api traffic stays forbidden here and goes over P2P only.
	// In single-tenant mode HomeWeb keeps its older meaning: serve static files
	// from disk instead of forwarding them to the Pi.
	HomeWeb string
	// HomePubKey is the pinned home-site ES256 public key (hex X||Y). When set,
	// the relay answers GET /api/identity for the home host directly from this
	// pin (SLICE 2) instead of forwarding to the Pi — the browser's TOFU anchor
	// is then served even while the Pi is offline.
	HomePubKey string
	// RequireDeviceKey ENFORCES the C2 device-key signaling gate. Off (default)
	// keeps the pre-C2 behaviour (park the raw offer, no proof) so the relay can
	// ship slices 1+2 while a home Pi that doesn't yet publish device-keys (C1)
	// still works. On → an offer must carry a verified device-key proof or the Pi
	// is never contacted. Flip on only once device-keys are enrolled.
	RequireDeviceKey bool
	// ICEStunURLs/TURNURLs are public connectivity hints for the signed
	// browser<->Pi WebRTC channel. TURN only relays DTLS ciphertext.
	ICEStunURLs []string
	TURNURLs    []string
	TURNSecret  string
	// MultiTenant switches the home host from the single-tenant pin
	// (-home-site/-home-pubkey) to the public multi-tenant front door:
	// anonymous home.* serves only the relay-disk bootstrap loader; after the
	// browser decrypts its directory, static app GETs are forwarded to the chosen
	// Pi by opaque site_id. Per-wallet ciphertext blobs are served from
	// WalletBlobs, and owner data routing is driven by /signal/* + P2P. The relay
	// never learns the passkey↔Pi mapping; it only sees the opaque site selected
	// by the browser once the owner has unlocked it.
	MultiTenant bool
	// WalletBlobs is the durable per-wallet ENCRYPTED directory store. The relay
	// never parses the ciphertext (all crypto is client-side via the WebAuthn-PRF
	// derived key); it only stores {ciphertext, nonce, version} keyed by an opaque
	// userHandle and serves it back over GET/PUT /wallet/{user_handle}/blob. Nil
	// outside multi-tenant mode.
	WalletBlobs *WalletBlobStore
	// Bootstrap is the EPHEMERAL, BLIND first-enrollment store: during the brief
	// onboarding window the Pi parks its own ES256-signed directory descriptor here
	// (keyed for claim by sha256(PIN)) so a fresh browser that knows the PIN can pull
	// it before the durable wallet blob exists. In-memory, TTL'd, and never
	// trust-parsed by the relay — see bootstrap.go / bootstrap_http.go. Nil outside
	// multi-tenant mode.
	Bootstrap *BootstrapStore
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
	if r.Challenges == nil {
		r.Challenges = NewSignalChallenges()
	}
	if r.OfferLimit == nil {
		// Per-source-IP token bucket on the unauthenticated browser offer endpoint
		// (FIX-C): bounds one abusive IP without letting it lock out a legit browser
		// on a different IP (which the old per-site limit did).
		r.OfferLimit = newIPRateLimiter(offerBucketCapacity, offerBucketRefillPerSec)
	}
	if r.ICELimit == nil {
		// SEPARATE bucket for GET /signal/ice (the TURN-credential mint) so an ICE
		// fetch never draws down the offer burst — both the browser (once per connect)
		// and the Pi (hourly) hit it, and a connect spends an ICE token AND an offer
		// token. A more generous bucket keeps a legit reconnect loop from 429ing.
		r.ICELimit = newIPRateLimiter(iceBucketCapacity, iceBucketRefillPerSec)
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
	mux.HandleFunc("GET /signal/{site_id}/challenge", r.signalChallenge)
	mux.HandleFunc("POST /signal/{site_id}/offer", r.signalBrowserOffer)
	mux.HandleFunc("GET /signal/{site_id}/answer", r.signalBrowserAnswer)
	mux.HandleFunc("GET /signal/{host_id}/offer", r.signalHostOffer)
	mux.HandleFunc("POST /signal/{host_id}/answer", r.signalHostAnswer)

	// Multi-tenant per-wallet ENCRYPTED directory blob (opaque ciphertext in/out,
	// never parsed by the relay) + the per-site public-key cross-check read. These
	// are registered ONLY in multi-tenant mode so that with the flag off they are
	// not even reachable (a 404, not a 503/public-key answer) — no extra surface
	// when the feature is dormant. Re-registered on the HomeHost mux below so they
	// aren't shadowed by the home-host catch-all.
	if r.MultiTenant {
		mux.HandleFunc("GET /wallet/{user_handle}/blob", r.walletBlobGet)
		mux.HandleFunc("PUT /wallet/{user_handle}/blob", r.walletBlobPut)
		mux.HandleFunc("OPTIONS /wallet/{user_handle}/blob", r.walletBlobOptions)
		mux.HandleFunc("GET /signal/{site_id}/identity", r.signalIdentity)
		// First-enrollment bootstrap: the Pi parks its signed directory descriptor
		// (PUT, site-key-authenticated) and a fresh browser claims it back by PIN
		// (POST, rate-limited). See bootstrap_http.go.
		mux.HandleFunc("PUT /bootstrap/{site_id}", r.bootstrapPut)
		mux.HandleFunc("POST /bootstrap/claim", r.bootstrapClaim)
		mux.HandleFunc("OPTIONS /bootstrap/claim", r.walletBlobOptions)
	}

	// Owner remote access registration (Phase 3). The Pi POSTs its ES256-signed
	// /me/register on startup; the relay pins the key and returns the per-host
	// poll secret the Pi uses BOTH to drain the (friend-flow) tunnel and to
	// authenticate the /signal/* rendezvous. This is control-plane only — there
	// is NO LONGER an owner HTTP request/response tunnel (the /me/<site>/... and
	// /me/<site> forwarders were removed in the P2P-only cutover, slice 6). Owner
	// data exists ONLY as DTLS DataChannel frames now.
	mux.HandleFunc("POST /me/register", r.meRegister)
	mux.HandleFunc("GET /signal/ice", r.signalICE)

	// Home-host cutover: a bare host (home.fortytwowatts.com) serves the stable
	// public bootstrap at the root. In multi-tenant mode the dashboard's STATIC
	// assets are forwarded to the browser-selected Pi only after the local
	// directory has supplied an opaque site_id routing cookie. In all modes this
	// catch-all is restricted to GET of NON-/api/ paths: owner API + the ftw_owner
	// cookie never traverse the relay — they ride the DTLS DataChannel only.
	// The home host registers when EITHER a single-tenant pin (-home-site) OR
	// multi-tenant mode is configured. The browser reaches the relay AS the home
	// host, and Go's ServeMux gives a host-specific pattern precedence over a
	// host-less one — so the host-less /signal/* + /wallet/* routes above would be
	// shadowed by the home-host catch-all below. Re-register every browser-facing
	// route on the home host explicitly so they resolve from the dashboard origin.
	// (The Pi's /signal/{host}/* routes need no host pin: the Pi dials the relay by
	// its own host, not the home host.)
	if r.HomeHost != "" && (r.HomeSite != "" || r.MultiTenant) {
		mux.HandleFunc("GET "+r.HomeHost+"/signal/ice", r.signalICE)
		mux.HandleFunc("GET "+r.HomeHost+"/signal/{site_id}/challenge", r.signalChallenge)
		mux.HandleFunc("POST "+r.HomeHost+"/signal/{site_id}/offer", r.signalBrowserOffer)
		mux.HandleFunc("GET "+r.HomeHost+"/signal/{site_id}/answer", r.signalBrowserAnswer)
		// The wallet-blob + per-site identity routes are multi-tenant-only — never
		// registered on the home host under a single-tenant pin.
		if r.MultiTenant {
			mux.HandleFunc("GET "+r.HomeHost+"/signal/{site_id}/identity", r.signalIdentity)
			mux.HandleFunc("GET "+r.HomeHost+"/wallet/{user_handle}/blob", r.walletBlobGet)
			mux.HandleFunc("PUT "+r.HomeHost+"/wallet/{user_handle}/blob", r.walletBlobPut)
			mux.HandleFunc("OPTIONS "+r.HomeHost+"/wallet/{user_handle}/blob", r.walletBlobOptions)
			mux.HandleFunc("PUT "+r.HomeHost+"/bootstrap/{site_id}", r.bootstrapPut)
			mux.HandleFunc("POST "+r.HomeHost+"/bootstrap/claim", r.bootstrapClaim)
			mux.HandleFunc("OPTIONS "+r.HomeHost+"/bootstrap/claim", r.walletBlobOptions)
			// Hardened first-enrollment bridge: the ONLY owner-API paths the relay
			// forwards to the Pi, and only while a live PIN-gated bootstrap blob
			// exists for the resolved site (single-use, rate-limited). These patterns
			// are MORE SPECIFIC than the home-host catch-all below, so Go's ServeMux
			// routes them here rather than to homeStaticForward's /api/* → 403. See
			// bootstrapEnrollForward.
			mux.HandleFunc("POST "+r.HomeHost+"/api/owner-access/enroll/start", r.bootstrapEnrollForward("start"))
			mux.HandleFunc("POST "+r.HomeHost+"/api/owner-access/enroll/finish", r.bootstrapEnrollForward("finish"))
		}
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

// homeStaticForward serves the bare home host. In multi-tenant mode anonymous
// browsers get only the relay-disk bootstrap allowlist; once the bootstrap has
// decrypted the owner's directory it writes ftw_home_site=<opaque site_id>, and
// this handler forwards STATIC app GETs to that Pi. In single-tenant mode the
// older -home-site/-home-web rules still apply.
//
// FAIL-CLOSED owner-data gate: it forwards only GET requests for NON-/api/ paths.
// The owner API and the ftw_owner session cookie therefore NEVER traverse the
// relay — they ride the DTLS DataChannel only (the P2P-only home route).
func (r *Relay) homeStaticForward(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Cookie")
	if r.Owners == nil {
		http.Error(w, "owner registry not configured", http.StatusServiceUnavailable)
		return
	}
	// FAIL-CLOSED owner-data gate. Only GET may traverse the relay for the home
	// host, and never an /api/ path — those carry owner data + the session cookie
	// and must travel inside DTLS only. Anything else is refused here so the
	// relay can never see (or be tricked into proxying) owner traffic.
	if req.Method != http.MethodGet {
		writeRemoteError(w, req, http.StatusMethodNotAllowed, errRemoteP2POnly,
			"owner/control requests must use the private P2P channel. For first passkey setup, open a fresh setup QR from local Access and use the https://home.fortytwowatts.com link.")
		return
	}
	// Reserve the multi-tenant API-plane paths. Under -multi-tenant these are
	// registered routes and never reach here; under a single-tenant pin they would
	// otherwise be swallowed by this catch-all (served as SPA HTML, or — in an
	// insecure TOFU-without-home-web config — forwarded to the Pi as an anonymous
	// GET). 404 them so the wallet/identity surface is never reachable via the
	// static path either (Codex 2026-06-05).
	if strings.HasPrefix(req.URL.Path, "/wallet/") ||
		(strings.HasPrefix(req.URL.Path, "/signal/") && strings.HasSuffix(req.URL.Path, "/identity")) {
		http.NotFound(w, req)
		return
	}
	// MULTI-TENANT: anonymous clients get only the relay-disk bootstrap allowlist.
	// After local directory unlock, ftw_home_site routes static app GETs to the
	// chosen Pi. The legacy /api/identity TOFU exception does NOT apply — under
	// multi-tenant identity is per-site at GET /signal/{site_id}/identity, so
	// /api/* (identity included) is uniformly refused here and the owner data
	// plane stays strictly P2P.
	if r.MultiTenant {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			writeRemoteError(w, req, http.StatusForbidden, errRemoteAPIP2POnly,
				"owner API is P2P-only; this relay serves only the bootstrap loader and static app files.")
			return
		}
		if req.URL.Query().Get("reset_remote") == "1" {
			clearHomeRouteSiteCookie(w)
			r.serveHomeBootstrapFile(w, req)
			return
		}
		if homeBootstrapAlwaysLocal(req.URL.Path) {
			r.serveHomeBootstrapFile(w, req)
			return
		}
		if siteID := homeRouteSiteFromCookie(req); siteID != "" {
			registered, ok := r.forwardHomeStaticGET(w, req, siteID)
			if ok {
				return
			}
			if registered {
				r.serveHomeOffline(w, req, true)
				return
			}
			clearHomeRouteSiteCookie(w)
			if req.URL.Path != "/" && req.URL.Path != "/index.html" {
				http.NotFound(w, req)
				return
			}
		}
		r.serveHomeBootstrapFile(w, req)
		return
	}
	// SLICE 2: serve GET /api/identity from the home pubkey the relay already
	// holds (the operator-provided -home-pubkey pin) WITHOUT forwarding to the Pi.
	// This is the browser's TOFU anchor — a public key only, no secret/cookie — so
	// the relay can answer it locally and the dashboard's bootstrap works even
	// while the Pi is mid-connect or offline. Only when a key is actually pinned;
	// otherwise fall through to the existing gate (which forwards it as the TOFU
	// exception).
	if req.URL.Path == "/api/identity" && r.HomePubKey != "" {
		r.serveHomeIdentity(w)
		return
	}
	if isOwnerAPIPath(req.URL.Path) {
		writeRemoteError(w, req, http.StatusForbidden, errRemoteAPIP2POnly,
			"owner API is P2P-only; this relay serves only the bootstrap loader and static app files.")
		return
	}
	// SINGLE-TENANT SLICE 1: when -home-web is set, serve the static shell from
	// the relay's own disk instead of forwarding the GET to the Pi. Multi-tenant
	// returned above and only serves the bootstrap allowlist from disk.
	if r.HomeWeb != "" {
		r.serveHomeStaticFile(w, req)
		return
	}
	registered, ok := r.forwardHomeStaticGET(w, req, r.HomeSite)
	if !ok {
		// Never connected, stopped re-registering, or failed mid-request: show a
		// reassuring styled page (not a raw timeout/503 that reads as "broken").
		r.serveHomeOffline(w, req, registered)
	}
}

// forwardHomeStaticGET forwards a safe static GET to the registered Pi for
// siteID. It returns registered=false when the site is unknown, and ok=false when
// the site is offline/stale or the tunnel request failed. The caller owns the
// fallback UI. /api and method checks are intentionally outside this helper so
// the fail-closed gate stays visually obvious in homeStaticForward.
func (r *Relay) forwardHomeStaticGET(w http.ResponseWriter, req *http.Request, siteID string) (registered bool, ok bool) {
	hostID, registered, fresh := r.Owners.Active(siteID, homeStaleAfter)
	if !registered || !fresh {
		return registered, false
	}
	innerPath := req.URL.Path
	if q := req.URL.RawQuery; q != "" {
		innerPath = innerPath + "?" + q
	}
	// Strip any inbound cookies: the owner session lives only inside DTLS, so a
	// stray ftw_owner on a static-asset GET must never reach the Pi over the relay.
	req.Header.Del("Cookie")
	resp, err := r.enqueue(req, hostID, innerPath, nil)
	if err != nil {
		return true, false
	}
	// Defence in depth: never relay a Set-Cookie from the Pi for the home host —
	// a static asset has no business setting the owner cookie, and the owner
	// session must never appear on a relay-visible response.
	writeTunneledNoCookie(w, resp, homeStaticCacheControl(req.URL))
	return true, true
}

// writeTunneledNoCookie copies a tunneled Pi response to the client with
// Set-Cookie STRIPPED. The owner session cookie (ftw_owner) lives only inside
// the DTLS DataChannel; it must NEVER appear on a relay-visible response, on any
// path that forwards to the Pi (static assets AND the bootstrap enroll-forward).
// This is the single chokepoint so the no-cookie-on-relay invariant is structural.
func writeTunneledNoCookie(w http.ResponseWriter, resp tunnel.TunneledResponse, cacheControl string) {
	resp.Header.Del("Set-Cookie")
	for k, vv := range resp.Header {
		w.Header()[k] = vv
	}
	if cacheControl == "" {
		cacheControl = "no-store"
	}
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Add("Vary", "Cookie")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

// homeStaticCacheControl keeps owner HTML/API surfaces uncached, but allows the
// browser to privately cache Pi-routed static assets. The relay remains blind and
// shared caches still stay out of it ("private" + Vary: Cookie); this just stops
// Safari from re-downloading the full dashboard bundle on every reload.
func homeStaticCacheControl(u *url.URL) string {
	if u == nil {
		return "no-store"
	}
	clean := path.Clean("/" + u.Path)
	if clean == "/" || clean == "/index.html" || strings.HasSuffix(clean, ".html") {
		return "no-store"
	}
	ext := strings.ToLower(path.Ext(clean))
	if ext == "" {
		return "no-store"
	}
	switch ext {
	case ".js", ".css", ".mjs", ".map", ".json", ".svg", ".png", ".jpg", ".jpeg", ".webp", ".ico", ".woff", ".woff2":
	default:
		return "no-store"
	}
	if u.Query().Get("v") != "" {
		return "private, max-age=86400"
	}
	return "private, max-age=300"
}

// serveHomeIdentity answers GET /api/identity for the home host from the pinned
// home pubkey the relay already holds (SLICE 2). It returns the same shape the Pi
// would, so the browser's TOFU bootstrap is byte-compatible whether the key comes
// from the relay pin or (in -home-pubkey-less back-compat) the forwarded Pi
// response. Public key only — no secret, no cookie — so serving it locally leaks
// nothing and lets the dashboard bootstrap even while the Pi is offline.
func (r *Relay) serveHomeIdentity(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	// Cache-Control: the pin is stable for the relay's lifetime, but keep it
	// short so a key rotation (operator restarts the relay with a new pin)
	// propagates quickly.
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		PublicKeyHex string `json:"public_key_hex"`
		SiteID       string `json:"site_id"`
		Algorithm    string `json:"algorithm"`
		Curve        string `json:"curve"`
	}{
		PublicKeyHex: r.HomePubKey,
		SiteID:       r.HomeSite,
		Algorithm:    "ES256",
		Curve:        "P-256",
	})
}

// walletBlobIO is the wire shape of GET/PUT /wallet/{user_handle}/blob. The
// relay treats ciphertext + nonce as OPAQUE standard-base64 strings — it never
// decodes the plaintext (all crypto is client-side, derived from a WebAuthn-PRF
// secret). version drives optimistic concurrency: a PUT whose version is <= the
// stored one is a 409 (lost-update / rollback guard). On PUT, write_pub + sig
// authenticate the writer: write_pub is the wallet's Ed25519 write key (also
// PRF-derived, TOFU-pinned by the relay) and sig is its signature over the
// canonical message (see WalletBlobStore.blobWriteMessage). They are absent on GET.
type walletBlobIO struct {
	Ciphertext string `json:"ciphertext"`
	Nonce      string `json:"nonce"`
	Version    int    `json:"version"`
	WritePub   string `json:"write_pub,omitempty"`
	Sig        string `json:"sig,omitempty"`
}

// walletBlobCORS sets permissive CORS on the per-wallet blob endpoints. They are
// safe to expose cross-origin: the blob is opaque ciphertext (worthless without
// the owner's PRF-derived key), a write is authenticated by the Ed25519 write
// signature (NOT by any cookie/credential), and the handle is strictly validated.
// The LAN enrollment page — served from the owner's OWN Pi origin — writes the
// first directory blob here cross-origin, so a permissive ACAO is required. No
// credentials are ever read, so "*" is safe (and credentials are not allowed).
func walletBlobCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, PUT, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Max-Age", "600")
}

// walletBlobOptions answers the CORS preflight for the blob endpoints.
func (r *Relay) walletBlobOptions(w http.ResponseWriter, req *http.Request) {
	walletBlobCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

// walletBlobGet serves a wallet's encrypted directory blob. Public + unauthenticated
// at the HTTP layer (the ciphertext is worthless without the user's PRF-derived
// key) but the handle is strictly validated so it can never be used for path
// traversal. Returns 400 (bad handle), 404 (no blob), or 200 with the opaque
// {ciphertext, nonce, version}. The relay never parses the plaintext.
func (r *Relay) walletBlobGet(w http.ResponseWriter, req *http.Request) {
	walletBlobCORS(w)
	handle := req.PathValue("user_handle")
	if !validUserHandle(handle) {
		http.Error(w, "invalid user_handle", http.StatusBadRequest)
		return
	}
	if r.WalletBlobs == nil {
		http.Error(w, "wallet blob store not configured", http.StatusServiceUnavailable)
		return
	}
	ct, nonce, version, ok := r.WalletBlobs.Get(handle)
	if !ok {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(walletBlobIO{
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Version:    version,
	})
}

// walletBlobPut stores a wallet's encrypted directory blob under optimistic
// concurrency, WRITER-AUTHENTICATED. The body is JSON {ciphertext, nonce,
// version, write_pub, sig} — ciphertext/nonce are standard-base64 the relay never
// decrypts; write_pub is the wallet's Ed25519 write key (base64) and sig (base64)
// is its signature over the canonical message. The store TOFU-pins write_pub on
// the first write and rejects any later write not signed by that key, so a
// userHandle-knower WITHOUT the owner's passkey-derived write key cannot overwrite
// or take over the blob. Statuses:
//   - 400  invalid handle / malformed JSON / non-base64 field
//   - 403  signature does not verify against the (pinned) write key
//   - 409  version <= stored (lost-update / rollback)
//   - 413  ciphertext over the per-blob byte cap
//   - 503  too many distinct wallets (store cap)
//   - 200  stored
func (r *Relay) walletBlobPut(w http.ResponseWriter, req *http.Request) {
	walletBlobCORS(w)
	handle := req.PathValue("user_handle")
	if !validUserHandle(handle) {
		http.Error(w, "invalid user_handle", http.StatusBadRequest)
		return
	}
	if r.WalletBlobs == nil {
		http.Error(w, "wallet blob store not configured", http.StatusServiceUnavailable)
		return
	}
	req.Body = http.MaxBytesReader(w, req.Body, maxControlBodyBytes)
	var in walletBlobIO
	if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
		http.Error(w, "malformed blob body", http.StatusBadRequest)
		return
	}
	ct, err := base64.StdEncoding.DecodeString(in.Ciphertext)
	if err != nil {
		http.Error(w, "ciphertext is not valid base64", http.StatusBadRequest)
		return
	}
	nonce, err := base64.StdEncoding.DecodeString(in.Nonce)
	if err != nil {
		http.Error(w, "nonce is not valid base64", http.StatusBadRequest)
		return
	}
	writePub, err := base64.StdEncoding.DecodeString(in.WritePub)
	if err != nil {
		http.Error(w, "write_pub is not valid base64", http.StatusBadRequest)
		return
	}
	sig, err := base64.StdEncoding.DecodeString(in.Sig)
	if err != nil {
		http.Error(w, "sig is not valid base64", http.StatusBadRequest)
		return
	}
	if err := r.WalletBlobs.Put(handle, ct, nonce, writePub, sig, in.Version); err != nil {
		switch {
		case errors.Is(err, ErrUnauthorizedWrite):
			http.Error(w, "write not authorized", http.StatusForbidden)
		case errors.Is(err, ErrVersionConflict):
			http.Error(w, "stale version", http.StatusConflict)
		case errors.Is(err, ErrBlobTooLarge):
			http.Error(w, "blob too large", http.StatusRequestEntityTooLarge)
		case errors.Is(err, ErrTooManyBlobs):
			http.Error(w, "relay at capacity", http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
}

// signalIdentity serves GET /signal/{site_id}/identity: the PUBLIC ES256 key the
// relay holds for a site (from OwnerRegistry), in the same shape /api/identity
// returns. It is a convenience cross-check only — the browser already learns the
// Pi pubkey from the Pi-SIGNED directory entry and pins from there, so a tampering
// relay cannot use this read to MITM the first connection (closing the old
// /api/identity TOFU gap). Public key only: no secret, no host_id, no routing. An
// unknown site (or empty site_id) is a 404 — anonymous callers learn nothing.
func (r *Relay) signalIdentity(w http.ResponseWriter, req *http.Request) {
	siteID := req.PathValue("site_id")
	if siteID == "" || r.Owners == nil {
		http.NotFound(w, req)
		return
	}
	pub, ok := r.Owners.PublicKeyForSite(siteID)
	if !ok {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(struct {
		PublicKeyHex string `json:"public_key_hex"`
		SiteID       string `json:"site_id"`
		Algorithm    string `json:"algorithm"`
		Curve        string `json:"curve"`
	}{
		PublicKeyHex: pub,
		SiteID:       siteID,
		Algorithm:    "ES256",
		Curve:        "P-256",
	})
}

// serveHomeStaticFile serves a static asset for the home host from the -home-web
// directory on the relay VM (SLICE 1), with path-traversal protection and an
// index.html fallback for "/". It NEVER forwards to the Pi. A missing file is a
// 404; "/" (or any path that resolves to a directory) serves index.html so the
// SPA shell loads at the root. The owner-API + method gates already ran in the
// caller, so only safe GETs of non-/api paths reach here.
func (r *Relay) serveHomeStaticFile(w http.ResponseWriter, req *http.Request) {
	// Clean the request path to an absolute, slash-rooted form, then strip the
	// leading slash so filepath.Join can't escape the web root. path.Clean
	// collapses any ".." so a crafted "/../../etc/passwd" resolves inside the
	// tree (or to "/"), never above it; the explicit prefix check below is belt
	// and braces.
	clean := path.Clean("/" + req.URL.Path)
	if clean == "/" {
		clean = "/index.html"
	}
	rel := strings.TrimPrefix(clean, "/")
	full := filepath.Join(r.HomeWeb, filepath.FromSlash(rel))
	// Defence in depth: confirm the resolved path is still within the web root
	// after symlink-free join. filepath.Join already cleaned ".." out of `rel`,
	// but a web root that is itself a relative path or a stray separator could
	// still surprise us — refuse anything that doesn't have the root as a prefix.
	root := filepath.Clean(r.HomeWeb)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		http.NotFound(w, req)
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		// Missing file or a directory request → serve the SPA shell so client-side
		// routes (deep links into the dashboard) resolve to index.html, the
		// conventional SPA fallback. If even index.html is missing, 404.
		index := filepath.Join(root, "index.html")
		if _, ierr := os.Stat(index); ierr != nil {
			http.NotFound(w, req)
			return
		}
		full = index
	}
	// Defence in depth against symlink escape (review finding): the lexical prefix
	// check above runs BEFORE symlink resolution, but http.ServeFile follows
	// symlinks — a link inside HomeWeb pointing outside would otherwise leak the
	// target. Resolve the final path + root and confirm the target truly stays
	// inside the web root.
	resolvedRoot, rerr := filepath.EvalSymlinks(root)
	resolved, ferr := filepath.EvalSymlinks(full)
	if rerr != nil || ferr != nil ||
		(resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator))) {
		http.NotFound(w, req)
		return
	}
	full = resolved
	// Never leak an owner cookie either direction: strip any inbound Cookie (the
	// session lives only in DTLS) — http.ServeFile sets no cookies itself.
	req.Header.Del("Cookie")
	http.ServeFile(w, req, full)
}

// serveHomeBootstrapFile serves ONLY the stable multi-tenant bootstrap allowlist
// from -home-web. It deliberately has no SPA fallback: a request for /next-app.js,
// /components/index.js, or any other dashboard asset without a chosen Pi must be
// 404, proving the relay is not shipping the app bundle. "/" maps to the remote
// loader, whose job is to decrypt the directory, set ftw_home_site, and reload so
// the real app shell is fetched from the Pi.
func (r *Relay) serveHomeBootstrapFile(w http.ResponseWriter, req *http.Request) {
	rel, ok := homeBootstrapRelPath(req.URL.Path)
	if !ok {
		http.NotFound(w, req)
		return
	}
	root := filepath.Clean(r.HomeWeb)
	full := filepath.Join(root, filepath.FromSlash(rel))
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		http.NotFound(w, req)
		return
	}
	resolvedRoot, rerr := filepath.EvalSymlinks(root)
	resolved, ferr := filepath.EvalSymlinks(full)
	if rerr != nil || ferr != nil ||
		(resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator))) {
		http.NotFound(w, req)
		return
	}
	req.Header.Del("Cookie")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Cookie")
	http.ServeFile(w, req, resolved)
}

func homeBootstrapAlwaysLocal(p string) bool {
	clean := path.Clean("/" + p)
	if clean == "/owner-access" || strings.HasPrefix(clean, "/owner-access/") {
		return true
	}
	switch clean {
	case "/p2p.js", "/remote-loader.js", "/vendor/qrcode.js":
		return true
	default:
		return false
	}
}

func homeBootstrapRelPath(p string) (string, bool) {
	clean := path.Clean("/" + p)
	switch clean {
	case "/", "/index.html", "/remote-loader.html":
		return "remote-loader.html", true
	case "/remote-loader.js":
		return "remote-loader.js", true
	case "/p2p.js":
		return "p2p.js", true
	case "/components/theme.css":
		return "components/theme.css", true
	case "/logo.jpg":
		return "logo.jpg", true
	case "/favicon.svg":
		return "favicon.svg", true
	case "/vendor/qrcode.js":
		return "vendor/qrcode.js", true
	case "/owner-access", "/owner-access/":
		return "owner-access/index.html", true
	}
	if strings.HasPrefix(clean, "/owner-access/") {
		rel := strings.TrimPrefix(clean, "/")
		switch rel {
		case "owner-access/index.html",
			"owner-access/enroll.html",
			"owner-access/login.html",
			"owner-access/webauthn.js",
			"owner-access/owner-fetch.js",
			"owner-access/device-key.js",
			"owner-access/prf.js",
			"owner-access/instance-sync.js",
			"owner-access/bootstrap-enroll.js",
			"owner-access/enroll-pin.js",
			"owner-access/setup-remote.js":
			return rel, true
		}
	}
	return "", false
}

func homeRouteSiteFromCookie(req *http.Request) string {
	c, err := req.Cookie(homeRouteSiteCookieName)
	if err != nil || c == nil || c.Value == "" {
		return ""
	}
	siteID, err := url.QueryUnescape(c.Value)
	if err != nil {
		return ""
	}
	if !validHomeRouteSiteID(siteID) {
		return ""
	}
	return siteID
}

func validHomeRouteSiteID(siteID string) bool {
	if len(siteID) < len("site:")+1 || len(siteID) > 200 || !strings.HasPrefix(siteID, "site:") {
		return false
	}
	for i := 0; i < len(siteID); i++ {
		c := siteID[i]
		if c <= 0x20 || c == 0x7f || c == '/' || c == '\\' || c == ';' || c == '"' {
			return false
		}
	}
	return true
}

func clearHomeRouteSiteCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     homeRouteSiteCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		SameSite: http.SameSiteLaxMode,
	})
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
	// ATOMICITY (escalation fix): the token and its poll-secret ownership must be
	// committed as a unit. If poll-secret ownership of host_id can't be proven
	// (the host_id's secret is bound to a DIFFERENT principal — e.g. it belongs to
	// an owner, or to another friend's pair token), this registration must leave
	// NO token behind. Otherwise an attacker could register T for a victim's
	// host_id H, get the 403, then approve T with their own code and have
	// /h/T/mcp route friend traffic to H — unauthorized access to the victim's Pi.
	//
	// We register the token, then prove poll-secret ownership; on ANY failure of
	// the ownership step we roll the token back (Delete) and release any secret
	// THIS call freshly minted, so a 403 is indistinguishable from "never
	// registered". The owner-prefix guard above already rejects owner host_ids
	// outright; this closes the same class for any non-owner host_id whose secret
	// is held by a different principal — and keeps the two steps atomic so the
	// ordering bug can't re-open it.
	pairPrincipal := "pair:" + reg.Token
	if _, err := r.Tokens.Register(TokenRegistration{
		HostID:       reg.HostID,
		Token:        reg.Token,
		TTL:          time.Duration(reg.TTLMs) * time.Millisecond,
		ApprovalCode: reg.ApprovalCode,
		Intent:       reg.Intent,
		As:           reg.As,
	}); err != nil {
		if errors.Is(err, ErrTooManyTokens) {
			http.Error(w, "relay at capacity", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	secret := ""
	if r.Polls != nil {
		// Bind the friend's poll secret to the pair token that minted it (the
		// principal). A register for the same host_id under a different token can't
		// retrieve this secret, and the owner path (principal = site key) can never
		// collide with it. Issue NEVER mints on a principal mismatch — it returns
		// the error untouched — so the only state to undo on the 403 path is the
		// token we just registered.
		s, err := r.Polls.Issue(reg.HostID, pairPrincipal)
		if err != nil {
			// The host_id's secret is held by a DIFFERENT principal (owner or other
			// friend). Roll the token back so the 403 leaves nothing for the
			// attacker to approve, then refuse.
			r.Tokens.Delete(reg.Token)
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
	// DevicePubkeys (C1) is the set of device keys the Pi trusts to signal +
	// mint a session for this site. The Pi publishes it on the SAME
	// ES256-signed registration, so the relay trusts the set exactly as far as
	// it trusts the (verified) registration signature. Each entry is an
	// uncompressed P-256 key as 128 lowercase hex (X||Y). Optional + omitempty:
	// a registration without it leaves the site's device-key set unchanged is
	// NOT the behaviour — see meRegister: the field, when PRESENT, replaces the
	// set; the Pi always sends its current set so the relay mirrors it exactly.
	DevicePubkeys []string `json:"device_pubkeys,omitempty"`
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
	// Verify the signature binds (site_id, host_id, ts) to the presented key — and
	// when the body carries device_pubkeys, that it ALSO covers that exact set (v2),
	// so a captured registration body cannot be replayed with a swapped/added device
	// key the relay would then trust for signaling (the v1 string did not cover the
	// array — CVE-class bug found in review). A v1-only signature is still accepted
	// (old Pi / no device keys), but then any device_pubkeys present did NOT sign and
	// the storage step below verified the same way drops them — an attacker can't
	// smuggle a key past a v1 signature.
	var msg string
	if len(reg.DevicePubkeys) > 0 {
		msg = tunnel.MeRegisterSigningStringV2(reg.SiteID, reg.HostID, reg.TsMs, reg.DevicePubkeys)
	} else {
		msg = tunnel.MeRegisterSigningString(reg.SiteID, reg.HostID, reg.TsMs)
	}
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
	// C1: mirror the Pi's trusted device-key set for this site. The registration
	// signature was just verified against the site's pinned key, so the relay
	// trusts this set exactly as far as it trusts the registration. The Pi sends
	// its FULL current set every time, so SetDeviceKeys REPLACES (not merges) —
	// a key the owner removed on the Pi disappears from the relay on the next
	// re-registration. Canonicalise + de-dup so the stored set compares
	// byte-for-byte with what the browser presents on the signaling offer (C2);
	// silently drop malformed entries rather than reject the whole registration
	// (a single bad key must not knock the Pi's tunnel mapping offline).
	dev := make([]string, 0, len(reg.DevicePubkeys))
	seenDev := make(map[string]bool, len(reg.DevicePubkeys))
	for _, k := range reg.DevicePubkeys {
		if !validDevicePubKeyHex(k) || seenDev[k] {
			continue
		}
		seenDev[k] = true
		dev = append(dev, k)
	}
	r.Owners.SetDeviceKeys(reg.SiteID, dev)
	// C1 → R3 zero-device burn: a non-empty device-key set means a device is now
	// enrolled for this site, so the first-enrollment bootstrap window MUST be
	// closed. Burning it here means a replayed/stale claim_key can never reach an
	// already-enrolled Pi via the enroll-forward — defence in depth on top of the
	// Pi's own enrollAllowed zero-device check. Guard for single-tenant (no store).
	if r.Bootstrap != nil && len(dev) > 0 {
		r.Bootstrap.Burn(reg.SiteID)
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

// signalProofSigningString is the canonical message the browser signs with a
// trusted device key to be allowed to forward a signaling offer (C2). Both ends
// MUST build it identically — the browser (WebCrypto, raw r||s, base64url) and
// the relay (verifyES256B64URL). Versioned ("v1") so it can evolve without a
// silent browser↔relay mismatch; the colon-delimited (site, nonce) binds the
// proof to THIS site and THIS single-use nonce, so a signature can't be lifted to
// another site or replayed against a fresh nonce.
func signalProofSigningString(siteID, nonce string) string {
	return "ftw-signal:v1:" + siteID + ":" + nonce
}

// verifyOfferDeviceProof enforces C2: the browser proved possession of a device
// key the Pi trusts. It returns true ONLY when (in this order, fail-closed):
//   - device_pubkey is a well-formed P-256 key in the site's published set (C1),
//   - sig verifies over "ftw-signal:v1:<site>:<nonce>" against that key, and
//   - the challenge nonce was known, unexpired, and not yet used (consumed here).
//
// The nonce is consumed LAST and only on full success, so a request that fails
// the key/sig check leaves the nonce spendable for the legitimate browser. A
// valid signature already requires knowing the unguessable 32-byte nonce, so the
// ordering doesn't create a probing oracle. Any failure → false → the caller
// returns 403 WITHOUT contacting the Pi.
func (r *Relay) verifyOfferDeviceProof(siteID, devicePubkey, nonce, sig string) bool {
	if !validDevicePubKeyHex(devicePubkey) || nonce == "" || sig == "" {
		return false
	}
	if !r.Owners.HasDeviceKey(siteID, devicePubkey) {
		return false
	}
	if !verifyES256B64URL(devicePubkey, signalProofSigningString(siteID, nonce), sig) {
		return false
	}
	// Consume the single-use nonce last: a replayed (device_pubkey, nonce, sig)
	// triple fails here because the nonce is already gone, even though key + sig
	// still check out.
	return r.Challenges.Consume(siteID, nonce)
}

// signalChallenge issues a single-use, short-TTL nonce the browser signs with a
// trusted device key on its subsequent POST /signal/{site}/offer (C2). It is
// unauthenticated (the nonce is worthless without a device key in the site's
// published set) but bounded: the store caps sites + per-site nonces so a flood
// can't grow relay memory.
func (r *Relay) signalChallenge(w http.ResponseWriter, req *http.Request) {
	siteID := req.PathValue("site_id")
	if siteID == "" {
		http.Error(w, "missing site_id", http.StatusBadRequest)
		return
	}
	if r.Challenges == nil {
		http.Error(w, "signaling not configured", http.StatusServiceUnavailable)
		return
	}
	// TODO(C2-on phase, review finding #5): throttle challenge issuance with a
	// SEPARATE per-IP limiter (sharing OfferLimit double-counts a legit
	// challenge+offer pair). Until the device-key gate is ENFORCED this endpoint is
	// inert (an evicted nonce gates nothing), so the DoS is not yet reachable.
	nonce, expMs, ok := r.Challenges.Issue(siteID)
	if !ok {
		http.Error(w, "signaling at capacity", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Nonce string `json:"nonce"`
		ExpMs int64  `json:"exp_ms"`
	}{Nonce: nonce, ExpMs: expMs})
}

// signalNonceHeader carries the rendezvous nonce the Pi echoes back to the
// browser on a drained offer (FIX-4a). The browser supplied it as ?n=<nonce>;
// the Pi reads it here and re-sends it on POST /signal/{host}/answer?n=<nonce>,
// so the relay routes the answer to the right per-nonce mailbox.
const signalNonceHeader = "X-FTW-Signal-Nonce"

// signalNonceRe bounds the opaque rendezvous nonce to a safe hex charset and
// length. It is a pure routing key (the relay never parses the SDP), so a tight
// charset both prevents abuse and keeps it usable as a map key + header value.
var signalNonceRe = regexp.MustCompile(`^[0-9a-fA-F]{8,64}$`)

func validSignalNonce(s string) bool {
	return signalNonceRe.MatchString(s)
}

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
	// FIX-4a: the opaque rendezvous nonce keys the per-(site,nonce) mailbox so an
	// attacker's offers land in their own slot and can't displace the legit
	// browser's answer. Required + bounded.
	nonce := req.URL.Query().Get("n")
	if !validSignalNonce(nonce) {
		http.Error(w, "missing or invalid rendezvous nonce", http.StatusBadRequest)
		return
	}
	// FIX-C: throttle PER SOURCE IP, not per site. The relay (unlike the Pi) sees
	// the browser's source IP, so a single abusive IP is bounded here while a legit
	// browser on a DIFFERENT IP is never pushed to 429 — closing the per-site
	// lockout lever. The per-(site) ceiling in ParkOffer remains as a generous
	// backstop. offerClientIP uses the un-spoofable RemoteAddr unless -trust-cf-ip
	// is set AND the peer is a validated Cloudflare edge IP (see cloudflare.go),
	// so the per-IP throttle stays effective behind Cloudflare.
	if r.OfferLimit != nil && !r.OfferLimit.Allow(r.offerClientIP(req)) {
		http.Error(w, "too many offers from your address", http.StatusTooManyRequests)
		return
	}
	if r.Signals == nil || r.Challenges == nil || r.Owners == nil {
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
	// ROLLOUT GATE: the device-key signaling gate (C2) is only ENFORCED when
	// -require-device-key is set. Until then the relay behaves exactly as it did
	// before C2 — it parks the raw offer body and forwards it, no device-key proof.
	// This lets the relay serve the shell + identity itself (slices 1+2, the
	// anonymous-FETCH surface closed) while a home Pi that hasn't yet been upgraded
	// to PUBLISH device-keys (C1) keeps working. Turn the flag on once device-keys
	// are enrolled to close the anonymous-SIGNALING surface too.
	if !r.RequireDeviceKey {
		var env struct {
			SDP string `json:"sdp"`
		}
		if err := json.Unmarshal(body, &env); err == nil && env.SDP != "" {
			body = []byte(env.SDP)
		}
		if err := r.Signals.ParkOffer(siteID, nonce, body); err != nil {
			if err == errSignalRateLimited {
				http.Error(w, "too many offers", http.StatusTooManyRequests)
				return
			}
			http.Error(w, "signaling at capacity", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// C2 (enforced): the offer body is a JSON envelope carrying the raw SDP PLUS a
	// device-key proof. The browser must prove possession of a device key the Pi
	// published (C1) by signing the single-use challenge nonce it fetched from
	// GET /signal/{site}/challenge. We verify {device_pubkey, nonce, sig} BEFORE
	// touching the mailbox, so a caller that can't prove a trusted device key
	// never causes the Pi to be contacted at all.
	var env struct {
		SDP          string `json:"sdp"`
		DevicePubkey string `json:"device_pubkey"`
		Nonce        string `json:"nonce"`
		Sig          string `json:"sig"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "malformed offer envelope", http.StatusBadRequest)
		return
	}
	if env.SDP == "" {
		http.Error(w, "empty offer", http.StatusBadRequest)
		return
	}
	if !r.verifyOfferDeviceProof(siteID, env.DevicePubkey, env.Nonce, env.Sig) {
		// Any proof failure — unknown/expired/replayed challenge nonce, a
		// device_pubkey not in the site's published set, or a bad signature — is a
		// flat 403, and crucially the Pi is NEVER contacted (we have not parked the
		// offer or woken an offer-poller). Fail-closed.
		http.Error(w, "device proof required", http.StatusForbidden)
		return
	}
	// Park ONLY the raw SDP for the Pi, exactly as before C2: the Pi drains the
	// raw SDP body (it never sees the device-proof envelope), so the Pi side is
	// unchanged by C2. The device proof is a relay-side gate only.
	if err := r.Signals.ParkOffer(siteID, nonce, []byte(env.SDP)); err != nil {
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
	nonce := req.URL.Query().Get("n")
	if !validSignalNonce(nonce) {
		http.Error(w, "missing or invalid rendezvous nonce", http.StatusBadRequest)
		return
	}
	if r.Signals == nil {
		http.Error(w, "signaling not configured", http.StatusServiceUnavailable)
		return
	}
	// TakeAnswer never allocates a mailbox for an unknown site/nonce, so a flood
	// of GET /signal/<random>/answer?n=<random> can't grow relay memory (FIX-3).
	data, ok := r.Signals.TakeAnswer(siteID, nonce, signalPollTimeout)
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
	data, nonce, ok := r.Signals.TakeOffer(siteID, signalPollTimeout)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Echo the rendezvous nonce so the Pi can POST its answer back under the same
	// per-nonce mailbox the browser is polling (FIX-4a).
	w.Header().Set(signalNonceHeader, nonce)
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
	// The Pi echoes the nonce the offer was drained under (FIX-4a) so the answer
	// lands in the same per-nonce mailbox the browser is polling.
	nonce := req.URL.Query().Get("n")
	if !validSignalNonce(nonce) {
		http.Error(w, "missing or invalid rendezvous nonce", http.StatusBadRequest)
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
	r.Signals.ParkAnswer(siteID, nonce, body)
	w.WriteHeader(http.StatusNoContent)
}
