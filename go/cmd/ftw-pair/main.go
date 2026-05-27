// ftw-pair is the host-side sidecar that exposes a forty-two-watts
// instance as an MCP server over the subetha relay tunnel.
//
// Spawned by `forty-two-watts pair`. Talks to the running main
// service via http://localhost:8080. Exposes MCP on :9999, forwarded
// through the subetha relay to the friend's laptop.
//
// Lifecycle: TTL-bound (default 4h). Hard kill at expiry. One active
// session per host.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var Version = "dev"

// relayAddrFlag is a package-level flag so tunnel.go can read it.
// Default: FTW_PAIR_RELAY env var, then relay.fortytwowatts.com.
var relayAddrFlag *string

func main() {
	version := flag.Bool("version", false, "print version and exit")
	apiBase := flag.String("api", "http://localhost:8080", "URL of the running forty-two-watts service")
	repoDir := flag.String("repo", "/opt/forty-two-watts", "Path to the 42W repo / install dir")
	stateDir := flag.String("state", "/var/lib/forty-two-watts", "Path to the configured state dir")
	configPath := flag.String("config", "/etc/forty-two-watts/config.yaml", "Path to config.yaml")
	userDriversDir := flag.String("user-drivers", "", "Path to PERSISTENT user-drivers directory. deploy_driver writes Lua files here so they survive docker image updates. Defaults to sibling-of-config drivers/ when empty.")
	addr := flag.String("addr", "127.0.0.1:9999", "Local MCP server bind address")
	ttl := flag.Duration("ttl", 4*time.Hour, "Session TTL")
	intent := flag.String("intent", "", "Owner-stated purpose for this session")
	as := flag.String("as", "", "Optional friend identity (logged in audit)")
	// -no-subetha (alias: -no-wormhole) skips the relay tunnel for local /
	// e2e testing — exposes MCP only on the local addr. The old name is kept
	// as a hidden alias so existing test scripts and the e2e harness don't break.
	noRelay := flag.Bool("no-relay", false, "Skip relay tunnel setup — MCP-only mode for testing/local use")
	// -no-subetha + -no-wormhole are kept as hidden aliases so existing
	// test scripts and the e2e harness continue to work.
	noSubetha := flag.Bool("no-subetha", false, "deprecated alias for -no-relay")
	noWormhole := flag.Bool("no-wormhole", false, "deprecated alias for -no-relay")
	stateless := flag.Bool("stateless", false, "Enable stateless MCP sessions (no initialize handshake required)")
	relayDefault := "https://relay.fortytwowatts.com"
	if env := os.Getenv("FTW_PAIR_RELAY"); env != "" {
		relayDefault = env
	}
	relayAddrFlag = flag.String("relay", relayDefault, "Relay base URL (e.g. http://localhost:7378 for local dev)")
	flag.Parse()

	if *version {
		fmt.Printf("ftw-pair %s\n", Version)
		os.Exit(0)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sess := NewSession(ctx, SessionConfig{TTL: *ttl, Intent: *intent, As: *as})
	defer sess.End("sidecar_exit")
	audit := NewAudit()
	scope := NewScope(*repoDir, *stateDir)

	tools := []Tool{
		NewFtwAPITool(*apiBase),
		NewReadFileTool(scope),
		NewWriteFileTool(scope, audit),
		NewListDirectoryTool(scope),
		NewRunCommandTool(scope),
		NewRestartMainServiceTool(*apiBase),
		NewTailServiceLogsTool(*apiBase),
		NewNetworkScanTool(),
		NewHTTPProbeTool(),
		NewModbusProbeTool(),
		NewModbusWriteTool(),
		NewMQTTObserveTool(),
		NewPCapCaptureTool(),
		NewDeployDriverTool(scope, audit, *apiBase, *configPath, *userDriversDir),
		NewSessionLogTool(sess, audit),
		NewSessionRemainingTool(sess),
		NewSessionEndTool(sess),
	}

	mcpSrv, err := StartMCP(ctx, MCPConfig{
		Addr: *addr, Stateless: *stateless, Session: sess, Audit: audit, Tools: tools,
	})
	if err != nil {
		slog.Error("start mcp", "err", err)
		os.Exit(1)
	}
	defer mcpSrv.Shutdown(context.Background())

	var pairCode string
	var tunHandle *TunnelHandle // nil in -no-relay mode
	if *noRelay || *noSubetha || *noWormhole {
		slog.Info("relay tunnel skipped", "mcp_addr", mcpSrv.Addr())
		pairCode = "local:" + mcpSrv.Addr()
	} else {
		handle, err := StartTunnelHost(ctx, mcpSrv.Addr(), *apiBase, *ttl, *intent, *as)
		if err != nil {
			slog.Error("start tunnel host", "err", err)
			os.Exit(1)
		}
		tunHandle = handle
		pairCode = handle.Token
		go handle.Host.Run(ctx)
		fmt.Fprintf(os.Stderr, "PAIR CODE: %s\n", handle.Token)
		fmt.Fprintf(os.Stderr, "PAIR URL:  %s\n", handle.PublicURL)
		fmt.Fprintf(os.Stderr, "APPROVAL CODE (tell host on voice): %s\n", handle.ApprovalCode)
	}
	fmt.Fprintf(os.Stderr, "TTL: %s — sidecar will exit at expiry\n", *ttl)

	if err := postPairStatus(*apiBase, pairCode, sess); err != nil {
		slog.Warn("post pair status", "err", err)
	}

	// Abort-poller: the owner can run `forty-two-watts pair --abort` which
	// POSTs /api/pair/abort on the main service, clearing the session store.
	// We poll GET /api/pair/status here; a 404 means the store was cleared
	// (abort was requested) and we end the session gracefully.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sess.Done():
				return
			case <-t.C:
				resp, err := http.Get(*apiBase + "/api/pair/status")
				if err != nil {
					continue
				}
				resp.Body.Close()
				if resp.StatusCode == http.StatusNotFound {
					sess.End("aborted_by_owner")
					return
				}
			}
		}
	}()

	// Heartbeat: re-POST /api/pair/status every 5 s with live tool_count
	// and last_tools so the dashboard <ftw-pair-card> shows real-time activity.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-sess.Done():
				return
			case <-t.C:
				// Refresh the relay's session-info snapshot before
				// posting so the dashboard sees fresh presence + state.
				// Best-effort — if the relay is unreachable the last
				// snapshot is kept and the dashboard surfaces "stale".
				if tunHandle != nil {
					tunHandle.RefreshSessionInfo(ctx)
				}
				_ = postPairStatusFull(*apiBase, pairCode, sess, audit, tunHandle)
			}
		}
	}()

	<-sess.Done()
	slog.Info("pair session ended", "reason", sess.ExitReason(), "tool_calls", audit.ToolCount())

	// Clear the dashboard's pair-status entry so the UI doesn't keep showing
	// the session as active after the sidecar has exited. Without this, a
	// session that ends on its own (TTL expiry, abort-poller, etc.) leaves a
	// stale entry — the dashboard says "active" while a friend hitting the
	// relay URL gets a 502 (the long-poll loop is already dead). Use a fresh
	// context since ctx is likely cancelled.
	cleanupReq, _ := http.NewRequest("POST", *apiBase+"/api/pair/abort", nil)
	cleanupReq.Header.Set("Content-Type", "application/json")
	cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer ccancel()
	if resp, err := http.DefaultClient.Do(cleanupReq.WithContext(cctx)); err == nil {
		resp.Body.Close()
	}
}

// postPairStatusFull is the heartbeat variant of postPairStatus: it
// includes live tool_count + last_tools + clients_connected, plus the
// relay-side metadata (PairURL + ApprovalCode + SessionState +
// LastActivityMs) so the dashboard <ftw-pair-card> can show the URL
// to share, the 4-digit voice-channel code, and live presence.
//
// All four relay fields are zero in -no-relay mode and the dashboard
// degrades gracefully (omits the URL row, hides the Allow form).
func postPairStatusFull(apiBase, code string, sess *Session, audit *Audit, tun *TunnelHandle) error {
	body := map[string]any{
		"session_id":        sess.ID,
		"code":              code,
		"intent":            sess.Intent(),
		"started_at":        sess.StartedAt.UTC().Format(time.RFC3339),
		"ttl_s":             int(sess.Remaining().Seconds()),
		"tool_count":        audit.ToolCount(),
		"last_tools":        audit.LastTools(5),
		"clients_connected":       0, // legacy field; presence now derived from last_activity_ms
		"pending_approvals_count": 0,
	}
	if tun != nil {
		body["pair_url"] = tun.PublicURL
		body["approval_code"] = tun.ApprovalCode
		if tun.LastInfo != nil {
			body["session_state"] = tun.LastInfo.State
			body["last_activity_ms"] = tun.LastInfo.LastActivityMs
			body["pending_approvals_count"] = tun.LastInfo.PendingApprovals
		}
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", apiBase+"/api/pair/status", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// postPairStatus tells the running 42W service about the current pair
// session so the dashboard's <ftw-pair-card> can render it. Best-effort —
// a failure here doesn't block the session.
func postPairStatus(apiBase, code string, sess *Session) error {
	body := map[string]any{
		"session_id": sess.ID,
		"code":       code,
		"intent":     sess.Intent(),
		"started_at": sess.StartedAt.UTC().Format(time.RFC3339),
		"ttl_s":      int(sess.Remaining().Seconds()),
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", apiBase+"/api/pair/status", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
