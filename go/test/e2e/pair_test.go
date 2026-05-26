package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPairFlow verifies that ftw-pair, run as a child process alongside
// a live main service, exposes a working MCP endpoint and that the
// session report eventually contains the tool calls we made.
//
// The subetha relay hop is skipped — we talk directly to the sidecar's
// localhost MCP listener. The real subetha transport is exercised by
// its own test in go/internal/subetha.
func TestPairFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}

	repo := repoRoot(t)
	pairBin := buildBinary(t, repo, "ftw-pair")
	mainBin := buildBinary(t, repo, "forty-two-watts")

	// Temp config / state.
	work := t.TempDir()
	stateDir := filepath.Join(work, "state")
	_ = os.MkdirAll(stateDir, 0o755)
	cfgPath := writeMinimalConfig(t, work, stateDir)

	mainCmd := exec.Command(mainBin,
		"-config", cfgPath,
		"-web", filepath.Join(repo, "web"),
	)
	mainCmd.Stdout = os.Stdout
	mainCmd.Stderr = os.Stderr
	if err := mainCmd.Start(); err != nil {
		t.Fatalf("start main: %v", err)
	}
	defer mainCmd.Process.Kill()
	waitForAPI(t, "http://127.0.0.1:8080/api/status")

	// Start the sidecar on a fixed high port (the sidecar doesn't accept :0).
	// -no-subetha skips the relay handshake so the test doesn't need a live relay.
	pairCmd := exec.Command(pairBin,
		"-addr", "127.0.0.1:19999",
		"-api", "http://127.0.0.1:8080",
		"-repo", repo,
		"-state", stateDir,
		"-config", cfgPath,
		"-ttl", "1m",
		"-intent", "e2e smoke",
		"-no-subetha",
		"-stateless",
	)
	pairCmd.Stdout = os.Stdout
	pairCmd.Stderr = os.Stderr
	if err := pairCmd.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	defer pairCmd.Process.Kill()
	waitForAPI(t, "http://127.0.0.1:19999/healthz")

	// Drive an MCP tools/call for ftw_api → /api/status.
	resp := callMCP(t, "http://127.0.0.1:19999/mcp", "ftw_api", map[string]any{
		"method": "GET", "path": "/api/status",
	})
	if !strings.Contains(resp, "mode") {
		t.Fatalf("expected /api/status body in ftw_api response, got: %s", resp)
	}

	// Render session_log and confirm it captured ftw_api + intent.
	log := callMCP(t, "http://127.0.0.1:19999/mcp", "session_log", map[string]any{})
	if !strings.Contains(log, "ftw_api") {
		t.Fatalf("session_log missing ftw_api entry:\n%s", log)
	}
	if !strings.Contains(log, "e2e smoke") {
		t.Fatalf("session_log missing intent:\n%s", log)
	}
}

func buildBinary(t *testing.T, repo, name string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", dst, "./cmd/"+name)
	cmd.Dir = filepath.Join(repo, "go")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	return dst
}

func waitForAPI(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("waitForAPI %s: timed out", url)
}

func callMCP(t *testing.T, url, tool string, args map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      tool,
			"arguments": args,
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mcp call: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return string(out)
}

func writeMinimalConfig(t *testing.T, dir, stateDir string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	contents := fmt.Sprintf(`site:
  name: e2e
fuse:
  max_amps: 16
  phases: 3
  voltage: 230
state:
  path: %s/state.db
  cold_dir: %s/cold
drivers: []
`, stateDir, stateDir)
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func repoRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}
