package e2e

import (
	"bufio"
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
// The relay hop is skipped — we talk directly to the sidecar's
// localhost MCP listener. The real relay transport is exercised by
// TestPairFlowThroughRelay below.
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

// TestPairFlowThroughRelay is the binary-level e2e for the new
// request-response relay (replaces the old subetha host-pool
// regression). Builds ftw-relay + ftw-pair + main service, registers
// a token via ftw-pair startup, approves it via the relay's
// /h/<token>/approve endpoint, and issues N sequential GET /web/...
// requests as the "friend" using plain http.DefaultClient.
//
// The N-sequential assertion preserves the spirit of the old
// host-pool starvation test — any leak in the long-poll loop, queue,
// or response routing would show up as a request timing out.
func TestPairFlowThroughRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}

	repo := repoRoot(t)
	pairBin := buildBinary(t, repo, "ftw-pair")
	relayBin := buildBinary(t, repo, "ftw-relay")
	mainBin := buildBinary(t, repo, "forty-two-watts")

	work := t.TempDir()
	stateDir := filepath.Join(work, "state")
	_ = os.MkdirAll(stateDir, 0o755)
	cfgPath := writeMinimalConfig(t, work, stateDir)

	// 1. Relay (HTTP mode, no TLS). The relay's mux serves /healthz,
	//    /tunnel/*, and /h/* — same family ftw-pair will register against.
	const relayAddr = "127.0.0.1:27778"
	relayCmd := exec.Command(relayBin, "-addr", relayAddr, "-poll-timeout", "5s")
	relayCmd.Stdout = os.Stdout
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relayCmd.Process.Kill()

	relayURL := "http://" + relayAddr
	waitForAPI(t, relayURL+"/healthz")

	// 2. 42W main service so ftw-pair has something to report to.
	mainCmd := exec.Command(mainBin, "-config", cfgPath, "-web", filepath.Join(repo, "web"))
	mainCmd.Stdout = os.Stdout
	mainCmd.Stderr = os.Stderr
	if err := mainCmd.Start(); err != nil {
		t.Fatalf("start main: %v", err)
	}
	defer mainCmd.Process.Kill()
	waitForAPI(t, "http://127.0.0.1:8080/api/status")

	// 3. ftw-pair pointed at the in-process relay.
	pairCmd := exec.Command(pairBin,
		"-addr", "127.0.0.1:29998",
		"-api", "http://127.0.0.1:8080",
		"-repo", repo,
		"-state", stateDir,
		"-config", cfgPath,
		"-ttl", "2m",
		"-intent", "relay e2e",
		"-relay", relayURL,
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

	pairCode, approvalCode := readPairCodeAndApproval(t, pairStderr)

	// 4. Friend side: approve the session via the relay endpoint.
	apvBody := strings.NewReader(`{"code":"` + approvalCode + `"}`)
	apvResp, err := http.Post(relayURL+"/h/"+pairCode+"/approve", "application/json", apvBody)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if apvResp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(apvResp.Body)
		t.Fatalf("approve status %d body %q", apvResp.StatusCode, body)
	}
	apvResp.Body.Close()

	// Give the host loop a beat to settle after the first response.
	time.Sleep(200 * time.Millisecond)

	// 5. N sequential requests through the relay → host → dashboard.
	const N = 10
	for i := 0; i < N; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", relayURL+"/h/"+pairCode+"/web/api/status", nil)
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

// readPairCodeAndApproval scans the ftw-pair stderr for both
// "PAIR CODE: …" and "APPROVAL CODE (tell host on voice): …".
func readPairCodeAndApproval(t *testing.T, r io.Reader) (pair, approval string) {
	t.Helper()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		line := scanner.Text()
		os.Stderr.WriteString(line + "\n")
		if strings.HasPrefix(line, "PAIR CODE: ") {
			pair = strings.TrimPrefix(line, "PAIR CODE: ")
		}
		if strings.HasPrefix(line, "APPROVAL CODE (tell host on voice): ") {
			approval = strings.TrimPrefix(line, "APPROVAL CODE (tell host on voice): ")
		}
		if pair != "" && approval != "" {
			go func() {
				for scanner.Scan() {
					os.Stderr.WriteString(scanner.Text() + "\n")
				}
			}()
			return pair, approval
		}
	}
	t.Fatal("pair code + approval not seen on ftw-pair stderr")
	return "", ""
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
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	// Strip GIT_* env vars so a parent pre-commit hook's GIT_DIR /
	// GIT_WORK_TREE does not confuse --show-toplevel when `go test`
	// is invoked transitively from `make verify` inside the hook.
	env := os.Environ()
	cleaned := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, "GIT_") {
			cleaned = append(cleaned, e)
		}
	}
	cmd.Env = cleaned
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	return strings.TrimSpace(string(out))
}
