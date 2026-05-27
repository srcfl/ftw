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
	mux.Handle("/", dashProxy)

	host := tunnel.NewHost(relayURL, hostID, mux)

	return &TunnelHandle{
		Host:         host,
		Token:        token,
		ApprovalCode: code,
		PublicURL:    fmt.Sprintf("%s/h/%s", relayURL, token),
		HostID:       hostID,
	}, nil
}
