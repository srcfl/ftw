// Package main — tunnel.go
//
// Replaces the old subetha shim. ftw-pair now exposes its MCP server
// (and the dashboard at *apiBase) to a friend's browser / Claude Code
// via the request-response relay at relay.fortytwowatts.com (or a
// local relay for development).
//
// See docs/goals/relay-as-tunnel.md and docs/relay-deploy.md.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// TunnelHandle wraps a tunnel.Host with the metadata the rest of
// ftw-pair needs (public URL, approval code, host-id).
type TunnelHandle struct {
	Host         *tunnel.Host
	Token        string
	ApprovalCode string
	PublicURL    string
	HostID       string
	RelayBase    string // base URL for /tunnel/sessions/<token>/info polling

	// LastInfo holds the most recent /tunnel/sessions/<token>/info
	// snapshot, refreshed by RefreshSessionInfo on the heartbeat
	// cycle. Nil until first successful poll.
	infoMu   sync.Mutex
	LastInfo *RelaySessionInfo
}

// RelaySessionInfo mirrors the relay's SessionInfo JSON.
type RelaySessionInfo struct {
	State            string `json:"state"`
	PendingApprovals int    `json:"pending_approvals"`
	LastActivityMs   int64  `json:"last_activity_ms"`
	ExpiresAtMs      int64  `json:"expires_at_ms"`
}

// RefreshSessionInfo polls the relay's /tunnel/sessions/<token>/info
// endpoint and updates LastInfo. Best-effort — a transient failure is
// logged at debug level and the previous snapshot is kept.
func (h *TunnelHandle) RefreshSessionInfo(ctx context.Context) {
	if h == nil || h.RelayBase == "" || h.Token == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		h.RelayBase+"/tunnel/sessions/"+h.Token+"/info", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return
	}
	defer resp.Body.Close()
	var info RelaySessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return
	}
	h.infoMu.Lock()
	h.LastInfo = &info
	h.infoMu.Unlock()
}

// StartTunnelHost registers a token with the relay and returns a handle
// whose Host.Run can be started from a goroutine. mcpAddr is the
// local MCP server's host:port (no scheme); apiBase is the dashboard
// URL (e.g. http://localhost:8080).
func StartTunnelHost(ctx context.Context, mcpAddr, apiBase string, ttl time.Duration, intent, as string) (*TunnelHandle, error) {
	relayURL := strings.TrimRight(*relayAddrFlag, "/")
	hostID := randomHostID()
	token := genWordToken()
	code := genApprovalCode()

	regBody, _ := json.Marshal(map[string]any{
		"host_id":       hostID,
		"token":         token,
		"ttl_ms":        ttl.Milliseconds(),
		"approval_code": code,
		"intent":        intent,
		"as":            as,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, relayURL+"/tunnel/register", bytes.NewReader(regBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("register with relay: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register status %d: %s", resp.StatusCode, body)
	}

	// Router: dispatch /mcp to the local MCP server, everything else
	// (the /web/* family already stripped of its prefix by the relay)
	// to the dashboard at apiBase.
	mcpURL, err := url.Parse("http://" + strings.TrimPrefix(strings.TrimPrefix(mcpAddr, "http://"), "https://"))
	if err != nil {
		return nil, fmt.Errorf("parse mcpAddr: %w", err)
	}
	apiBaseURL, err := url.Parse(apiBase)
	if err != nil {
		return nil, fmt.Errorf("parse apiBase: %w", err)
	}
	mcpProxy := httputil.NewSingleHostReverseProxy(mcpURL)
	dashProxy := httputil.NewSingleHostReverseProxy(apiBaseURL)
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpProxy)
	mux.Handle("/mcp/", mcpProxy)
	// The friend's browser must not reach owner-only control surfaces either —
	// e.g. POST /api/pair/status would let a friend forge the owner's pair-card.
	mux.Handle("/", denyOwnerOnly(dashProxy))

	host := tunnel.NewHost(relayURL, hostID, mux)

	return &TunnelHandle{
		Host:         host,
		Token:        token,
		ApprovalCode: code,
		PublicURL:    fmt.Sprintf("%s/h/%s", relayURL, token),
		HostID:       hostID,
		RelayBase:    relayURL,
	}, nil
}

// isOwnerOnlyPath reports whether a path is an owner-only control surface that a
// friend pair-flow request must never reach through the sidecar's proxies:
// pairing control (a friend could forge the owner's pair-card state) and
// owner-access credential management. Shared by the ftw_api tool and the
// dashboard proxy. The genuine sidecar posts its own /api/pair/status DIRECTLY
// to the Pi (not through these friend proxies), so it is unaffected.
func isOwnerOnlyPath(p string) bool {
	return strings.HasPrefix(p, "/api/pair/") || strings.HasPrefix(p, "/api/owner-access/")
}

// denyOwnerOnly wraps a handler to refuse any owner-only path before forwarding.
func denyOwnerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOwnerOnlyPath(r.URL.Path) {
			http.Error(w, "not permitted over a friend session", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
