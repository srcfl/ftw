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
// registered as the owner over the relay.
//
// In the P2P-only home route (slices 4-6) the owner API no longer traverses the
// relay AT ALL: the home-host forwarder serves static GETs only and FAIL-CLOSED
// refuses every /api/* path (403). So a request via the relay's home-host route
// for an owner API endpoint MUST be refused at the relay (403) — and, as before,
// a 200 would mean the dashboard is exposed to the public internet. This is the
// regression guard for the exposure incident, now strengthened: the owner API
// is sealed off the relay entirely rather than merely gated 401 on the Pi.
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
		"FTW_REMOTE_ACCESS_ENABLED=true", // opt in (default off; FTW_RELAY_URL alone no longer dials)
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
		t.Fatalf("EXPOSURE: home-host /api/status returned 200 — the owner API must NEVER be served over the relay (P2P-only). body=%q", body)
	}
	// Post-slice-6: the owner API is refused at the relay (403, P2P-only), never
	// forwarded to the Pi. (Pre-cutover this was 401 from the Pi's gate; the new
	// 403-at-relay is a strictly stronger seal — the request never leaves the
	// relay edge.)
	if code != http.StatusForbidden {
		t.Fatalf("home-host /api/status: got %d, want 403 (owner API is P2P-only, refused at relay). body=%q", code, body)
	}

	// Belt-and-braces: /api/identity (the public-key TOFU anchor) IS reachable
	// over the relay so the browser can pin before opening the P2P channel. Poll
	// until the Pi's registration has propagated (503 = home offline / not yet
	// routed), then assert 200.
	idDeadline := time.Now().Add(20 * time.Second)
	var idCode int
	var idBody string
	for time.Now().Before(idDeadline) {
		req, _ := http.NewRequest("GET", relayURL+"/api/identity", nil)
		req.Host = "home.test"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		idCode, idBody = resp.StatusCode, string(b)
		if idCode == http.StatusServiceUnavailable || idCode == http.StatusBadGateway {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		break
	}
	if idCode != http.StatusOK {
		t.Fatalf("/api/identity over relay: got %d, want 200 (TOFU anchor). body=%q", idCode, idBody)
	}
}
