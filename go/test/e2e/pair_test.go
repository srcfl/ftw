package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

	// Same call via the REST surface — this is the path agents will actually use.
	restResp := callREST(t, "http://127.0.0.1:19999/tools/ftw_api", map[string]any{
		"method": "GET", "path": "/api/status",
	})
	if !strings.Contains(restResp, "mode") {
		t.Fatalf("REST: expected /api/status body, got: %s", restResp)
	}

	// REST catalog must list ftw_api.
	catalog := getREST(t, "http://127.0.0.1:19999/tools")
	if !strings.Contains(catalog, `"name":"ftw_api"`) {
		t.Fatalf("REST: /tools missing ftw_api entry:\n%s", catalog)
	}

	// Render session_log and confirm it captured both calls + intent.
	log := callMCP(t, "http://127.0.0.1:19999/mcp", "session_log", map[string]any{})
	if !strings.Contains(log, "ftw_api") {
		t.Fatalf("session_log missing ftw_api entry:\n%s", log)
	}
	if !strings.Contains(log, "e2e smoke") {
		t.Fatalf("session_log missing intent:\n%s", log)
	}
}

// TestPairFlowThroughRelay is the full-stack regression for the host-pool
// starvation bug. It runs a real ftw-subetha relay in-process, points
// ftw-pair (host) and ftw-connect (client) at it, then issues N sequential
// GET /healthz requests through the tunnel — every one MUST return 200.
//
// Before the relay-splice fix, the 5th request would return 000 because
// the host's worker pool was deadlocked in pipeConns (the relay's splice
// never closed the host side when the client closed, so the host's read
// from the keep-alive HTTP conn blocked forever).
func TestPairFlowThroughRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}

	repo := repoRoot(t)
	pairBin := buildBinary(t, repo, "ftw-pair")
	relayBin := buildBinary(t, repo, "ftw-subetha")
	connectBin := buildBinary(t, repo, "ftw-connect")
	mainBin := buildBinary(t, repo, "forty-two-watts")

	work := t.TempDir()
	stateDir := filepath.Join(work, "state")
	_ = os.MkdirAll(stateDir, 0o755)
	cfgPath := writeMinimalConfig(t, work, stateDir)

	// 1. Relay on a random localhost port.
	relayCmd := exec.Command(relayBin, "-addr", "127.0.0.1:27777")
	relayCmd.Stdout = os.Stdout
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relayCmd.Process.Kill()

	// Wait until the relay's TCP listener is up.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:27777", 200*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 2. 42W main service so ftw-pair has something to report to.
	mainCmd := exec.Command(mainBin, "-config", cfgPath, "-web", filepath.Join(repo, "web"))
	mainCmd.Stdout = os.Stdout
	mainCmd.Stderr = os.Stderr
	if err := mainCmd.Start(); err != nil {
		t.Fatalf("start main: %v", err)
	}
	defer mainCmd.Process.Kill()
	waitForAPI(t, "http://127.0.0.1:8080/api/status")

	// 3. ftw-pair pointed at the in-process relay. Capture stderr to a pipe
	//    so we can extract the PAIR CODE the sidecar prints.
	pairCmd := exec.Command(pairBin,
		"-addr", "127.0.0.1:29998",
		"-api", "http://127.0.0.1:8080",
		"-repo", repo,
		"-state", stateDir,
		"-config", cfgPath,
		"-ttl", "2m",
		"-intent", "relay e2e",
		"-relay-addr", "127.0.0.1:27777",
		"-stateless",
	)
	pairStderr, err := pairCmd.StderrPipe()
	if err != nil {
		t.Fatalf("pair stderr pipe: %v", err)
	}
	pairCmd.Stdout = os.Stdout
	if err := pairCmd.Start(); err != nil {
		t.Fatalf("start pair: %v", err)
	}
	defer pairCmd.Process.Kill()

	pairCode := readPairCode(t, pairStderr)

	// 4. ftw-connect — picks its own random localhost port for the tunnel.
	connectCmd := exec.Command(connectBin,
		"-relay-addr", "127.0.0.1:27777",
		pairCode,
	)
	connectStdout, err := connectCmd.StdoutPipe()
	if err != nil {
		t.Fatalf("connect stdout pipe: %v", err)
	}
	connectCmd.Stderr = os.Stderr
	if err := connectCmd.Start(); err != nil {
		t.Fatalf("start connect: %v", err)
	}
	defer connectCmd.Process.Kill()

	tunnelURL := readTunnelURL(t, connectStdout)

	// Give the connect listener a beat to bind before we hammer it.
	time.Sleep(200 * time.Millisecond)

	// 5. The actual regression assertion — N sequential requests through the tunnel.
	const N = 10
	for i := 0; i < N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", tunnelURL+"/healthz", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			t.Fatalf("req %d/%d failed: %v", i+1, N, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("req %d/%d: status %d body %q", i+1, N, resp.StatusCode, body)
		}
	}
}

// readPairCode scans the ftw-pair stderr for the "PAIR CODE: …" line.
func readPairCode(t *testing.T, r io.Reader) string {
	t.Helper()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		os.Stderr.WriteString(line + "\n")
		if strings.HasPrefix(line, "PAIR CODE: ") {
			code := strings.TrimPrefix(line, "PAIR CODE: ")
			// Drain remaining ftw-pair stderr in a background goroutine so the
			// pipe doesn't fill up.
			go func() {
				for scanner.Scan() {
					os.Stderr.WriteString(scanner.Text() + "\n")
				}
			}()
			return code
		}
	}
	t.Fatal("pair code not seen on ftw-pair stderr")
	return ""
}

// readTunnelURL scans ftw-connect stdout for "Tunnel ready: …".
func readTunnelURL(t *testing.T, r io.Reader) string {
	t.Helper()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		os.Stdout.WriteString(line + "\n")
		if strings.HasPrefix(line, "Tunnel ready: ") {
			url := strings.TrimPrefix(line, "Tunnel ready: ")
			go func() {
				for scanner.Scan() {
					os.Stdout.WriteString(scanner.Text() + "\n")
				}
			}()
			return url
		}
	}
	t.Fatal("tunnel URL not seen on ftw-connect stdout")
	return ""
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

func callREST(t *testing.T, url string, args map[string]any) string {
	t.Helper()
	body, _ := json.Marshal(args)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("REST call: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("REST call %s: status %d body %s", url, resp.StatusCode, out)
	}
	return string(out)
}

func getREST(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("REST get: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("REST get %s: status %d body %s", url, resp.StatusCode, out)
	}
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
