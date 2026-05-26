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

// relayAddrFlag is a package-level flag so subetha.go can read it.
// Default: FTW_PAIR_RELAY env var, then subetha.fortytwowatts.com:7777.
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
	noSubetha := flag.Bool("no-subetha", false, "Skip subetha relay setup — MCP-only mode for testing/local use")
	noWormhole := flag.Bool("no-wormhole", false, "deprecated alias for -no-subetha")
	stateless := flag.Bool("stateless", false, "Enable stateless MCP sessions (no initialize handshake required)")
	relayAddrFlag = flag.String("relay-addr", "", "Relay server address (overrides FTW_PAIR_RELAY env var and default subetha.fortytwowatts.com:7777)")
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
	if *noSubetha || *noWormhole {
		slog.Info("subetha relay skipped", "mcp_addr", mcpSrv.Addr())
		pairCode = "local:" + mcpSrv.Addr()
	} else {
		host, err := StartSubethaHost(ctx, mcpSrv.Addr())
		if err != nil {
			slog.Error("subetha host", "err", err)
			os.Exit(1)
		}
		defer host.Close()
		pairCode = host.Code
		fmt.Fprintf(os.Stderr, "PAIR CODE: %s\n", host.Code)
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
				_ = postPairStatusFull(*apiBase, pairCode, sess, audit)
			}
		}
	}()

	<-sess.Done()
	slog.Info("pair session ended", "reason", sess.ExitReason(), "tool_calls", audit.ToolCount())

	// Clear the dashboard's pair-status entry so the UI doesn't keep showing
	// the session as active after the sidecar has exited. Without this, a
	// session that ends on its own (TTL expiry, abort-poller, etc.) leaves a
	// stale entry — the dashboard says "active" while ftw-connect on the
	// friend side gets "no host ready" from the relay (the host workers are
	// already dead). Use a fresh context since ctx is likely cancelled.
	cleanupReq, _ := http.NewRequest("POST", *apiBase+"/api/pair/abort", nil)
	cleanupReq.Header.Set("Content-Type", "application/json")
	cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer ccancel()
	if resp, err := http.DefaultClient.Do(cleanupReq.WithContext(cctx)); err == nil {
		resp.Body.Close()
	}
}

// postPairStatusFull is the heartbeat variant of postPairStatus: it
// includes live tool_count + last_tools so the dashboard <ftw-pair-card>
// can show real-time activity.
func postPairStatusFull(apiBase, code string, sess *Session, audit *Audit) error {
	body := map[string]any{
		"session_id": sess.ID,
		"code":       code,
		"intent":     sess.Intent(),
		"started_at": sess.StartedAt.UTC().Format(time.RFC3339),
		"ttl_s":      int(sess.Remaining().Seconds()),
		"tool_count": audit.ToolCount(),
		"last_tools": audit.LastTools(5),
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
