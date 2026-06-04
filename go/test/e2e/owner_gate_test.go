package e2e

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestOwnerGateThroughRelay reproduces the home.fortytwowatts.com path end to
// end with REAL binaries: ftw-relay (home-host routing) + the main service
// registered as the owner over the relay tunnel. A request via the relay's
// home-host route with NO session MUST be refused by the auth-gate (401) — a
// 200 means the dashboard is exposed to the public internet. This is the
// regression guard for the exposure incident.
func TestOwnerGateThroughRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}
	repo := repoRoot(t)
	relayBin := buildBinary(t, repo, "ftw-relay")
	mainBin := buildBinary(t, repo, "forty-two-watts")

	work := t.TempDir()
	stateDir := filepath.Join(work, "state")
	_ = os.MkdirAll(stateDir, 0o755)
	apiPort := freePort(t)
	apiURL := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	cfgPath := writeMinimalConfig(t, work, stateDir, apiPort) // site.name = e2e → site:e2e

	// Relay with home-host routing → site:e2e.
	relayAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	// -home-allow-tofu: this test exercises the Pi's signed registration + the
	// relay's trust-on-first-use pin (it can't know the Pi's pubkey up front), so
	// it opts into TOFU rather than passing -home-pubkey.
	relayCmd := exec.Command(relayBin, "-addr", relayAddr, "-poll-timeout", "5s",
		"-home-host", "home.test", "-home-site", "site:e2e", "-home-allow-tofu")
	relayCmd.Stdout = os.Stdout
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relayCmd.Process.Kill()
	relayURL := "http://" + relayAddr
	waitForAPI(t, relayURL+"/healthz")

	// Main service registers with the relay as the owner. LAN bypass ON (the
	// production default); the gate must STILL refuse the relay (remote) path.
	mainCmd := exec.Command(mainBin, "-config", cfgPath, "-web", filepath.Join(repo, "web"))
	mainCmd.Env = append(os.Environ(),
		"FTW_RELAY_URL="+relayURL,
		"FTW_OWNER_ACCESS_LAN_BYPASS=true",
		"FTW_OWNER_ACCESS_RPID=home.test",
	)
	mainCmd.Stdout = os.Stdout
	mainCmd.Stderr = os.Stderr
	if err := mainCmd.Start(); err != nil {
		t.Fatalf("start main: %v", err)
	}
	defer mainCmd.Process.Kill()
	waitForAPI(t, apiURL+"/api/status")

	// Poll the home-host route until the owner registration has propagated
	// (503/502 = not routed yet), then assert the gate verdict.
	deadline := time.Now().Add(25 * time.Second)
	var code int
	var body string
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", relayURL+"/api/status", nil)
		req.Host = "home.test" // trigger the relay's home-host routing
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		code, body = resp.StatusCode, string(b)
		if code != http.StatusServiceUnavailable && code != http.StatusBadGateway {
			break // registered + routed through the tunnel
		}
		time.Sleep(500 * time.Millisecond)
	}

	if code == http.StatusOK {
		t.Fatalf("EXPOSURE: home-host /api/status returned 200 with NO session — the gate failed to treat the relay request as remote. body=%q", body)
	}
	if code != http.StatusUnauthorized {
		t.Fatalf("home-host /api/status: got %d, want 401 (gated). body=%q", code, body)
	}
}
