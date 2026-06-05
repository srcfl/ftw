package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// relaySigner is the ES256 identity used to authenticate owner-access relay
// registrations. Satisfied by *nova.Identity (the self-sovereign site key
// loaded in main, independent of whether Nova federation is enabled).
type relaySigner interface {
	PublicKeyHex() string
	SignRawHex(msg string) (string, error)
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
func runOwnerRelayRegistration(ctx context.Context, relayURL, siteID, hostID, tunnelMarker string, apiPort int, signer relaySigner) {
	relayURL = strings.TrimRight(relayURL, "/")

	// The public key is not a secret; log it in full so an operator can pin it
	// on the relay with -home-pubkey (closes the post-restart TOFU race).
	slog.Info("owner-access: relay registration identity",
		"site_id", siteID, "public_key", signer.PublicKeyHex())

	// Build the long-poll host up front so the registration loop can hand it the
	// relay-minted poll token. It starts polling immediately; until the first
	// registration sets the token its polls are 401'd and it backs off + retries.
	host := buildOwnerHost(relayURL, hostID, tunnelMarker, apiPort)
	go host.Run(ctx)

	registerOnce := func() {
		tsMs := time.Now().UnixMilli()
		sig, err := signer.SignRawHex(tunnel.MeRegisterSigningString(siteID, hostID, tsMs))
		if err != nil {
			slog.Warn("owner-access: sign register", "err", err)
			return
		}
		body, _ := json.Marshal(map[string]any{
			"site_id":    siteID,
			"host_id":    hostID,
			"public_key": signer.PublicKeyHex(),
			"ts_ms":      tsMs,
			"sig":        sig,
		})
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
		// Update the host's poll token (a relay restart re-mints it, so refresh
		// on every registration).
		host.SetPollSecret(out.PollSecret)
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

// buildOwnerHost constructs (but does not run) the host-side long-poll host for
// the owner-access tunnel. It pulls tunneled requests for this hostID and
// reverse-proxies them to the local API server on localhost. The local server's
// /api/owner-access/* handlers + cookie-based middleware validate WebAuthn
// assertions and gate access to /web /mcp paths.
//
// We use a fresh net/http/httputil.ReverseProxy so cookies (Set-Cookie on
// /login/finish, Cookie on subsequent requests) survive the tunnel roundtrip.
// The caller starts host.Run and feeds it the relay-minted poll token via
// host.SetPollSecret once registration returns it.
func buildOwnerHost(relayURL, hostID, tunnelMarker string, apiPort int) *tunnel.Host {
	// Connect to the local API server on its configured port.
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", apiPort))
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Stamp every forwarded (relay-tunnelled) request with the per-process
	// marker so the local API auth-gate treats it as remote, not LAN. Set()
	// overwrites any value a malicious browser tried to smuggle through the
	// header-preserving tunnel.
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		req.Header.Set("X-FTW-Tunnel", tunnelMarker)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, fmt.Sprintf("local api unavailable: %v", err), http.StatusBadGateway)
	}
	return newOwnerTunnelHost(relayURL, hostID, &ownerProxyHandler{proxy: proxy})
}

type ownerProxyHandler struct {
	proxy *httputil.ReverseProxy
}

func (o *ownerProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.proxy.ServeHTTP(w, r)
}
