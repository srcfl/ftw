package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/api"
	"github.com/frahlg/forty-two-watts/go/internal/config"
	"github.com/frahlg/forty-two-watts/go/internal/nova"
	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/frahlg/forty-two-watts/go/internal/telemetry"
	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

// TestBootstrapOnboardingThroughRelay is the full-stack e2e for the multi-tenant
// onboarding bootstrap on a HIGH-ENTROPY bootstrap_id (R1-R5 end state). It wires
// the REAL ftw-relay binary in -multi-tenant mode to a REAL in-process Pi
// (api.Server + its self-sovereign nova.Identity) and walks the whole courier:
//
//	mint PIN + bootstrap_id on the (simulated-LAN) Pi
//	  -> Pi self-publishes its signed descriptor keyed by hex(sha256(bootstrap_id)) + ts_ms
//	  -> a fresh client computes claim_key from the bootstrap_id, claims the
//	     descriptor back through the relay, and verifies its INNER sig exactly as the
//	     browser's verifyEntry would (base64url r||s ECDSA-P256 over the instance string)
//	  -> the client enrolls via the relay forward (?claim_key gate + ?pin Pi factor),
//	     and the REAL Pi enroll handler validates the PIN and opens the ceremony
//	  -> C2 fail-closed: a no-claim_key forward is refused at the relay, and a
//	     claim_key-but-no-PIN forward is refused at the Pi.
//
// Two facts shape what is asserted here (and are documented, not papered over):
//   - mintEnrollPin requires a genuine PRIVATE-RANGE LAN source (isLANClientSource
//     rejects loopback). We therefore drive the PIN-mint over the in-process handler
//     with a spoofed private-range RemoteAddr — that is the real Pi code path,
//     including the goroutine self-publish to the relay.
//   - The relay's enroll-forward enqueues onto the host's tunnel queue and the Pi's
//     enrollAllowed tunneled branch keys on the X-FTW-Tunnel marker. In production
//     the forwarder that drains enroll/* must stamp that marker; the main binary's
//     current static-asset host does NOT (it 403s every /api/*). This test stands in
//     the correct production wiring (a marker-stamping tunnel host serving the enroll
//     endpoints) so the relay<->Pi enroll contract is exercised end to end. See the
//     DONE_WITH_CONCERNS note in the task report: the main.go host wiring is the gap.
func TestBootstrapOnboardingThroughRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}
	repo := repoRoot(t)
	relayBin := buildBinary(t, repo, "ftw-relay")

	const siteID = "site:e2e"
	const siteName = "e2e"

	// ---- Pi identity (self-sovereign ES256 key) ----
	idPath := filepath.Join(t.TempDir(), "identity.key")
	identity, err := nova.LoadOrCreateIdentity(idPath)
	if err != nil {
		t.Fatalf("load identity: %v", err)
	}

	// ---- Relay (real binary), multi-tenant ----
	homeWeb := t.TempDir()
	if err := os.WriteFile(filepath.Join(homeWeb, "index.html"), []byte("<html>shell</html>"), 0o644); err != nil {
		t.Fatalf("write home shell: %v", err)
	}
	relayAddr := fmt.Sprintf("127.0.0.1:%d", freePort(t))
	relayURL := "http://" + relayAddr
	relayCmd := exec.Command(relayBin,
		"-addr", relayAddr,
		"-poll-timeout", "2s",
		"-multi-tenant",
		"-require-device-key", // -multi-tenant refuses to run without the C2 gate
		"-home-host", "home.test",
		"-home-web", homeWeb,
		"-wallet-blob-dir", t.TempDir(),
		"-home-allow-tofu",
	)
	relayCmd.Stdout = os.Stdout
	relayCmd.Stderr = os.Stderr
	if err := relayCmd.Start(); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer relayCmd.Process.Kill()
	waitForAPI(t, relayURL+"/healthz")

	// ---- Pi (in-process api.Server) ----
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()
	cfg := &config.Config{}
	cfg.Site.Name = siteName
	var capMu, cfgMu sync.RWMutex
	var ctrlMu sync.Mutex
	deps := &api.Deps{
		State: st,
		Tel:   telemetry.NewStore(),
		CapMu: &capMu, CfgMu: &cfgMu, CtrlMu: &ctrlMu,
		Capacities: map[string]float64{},
		Cfg:        cfg,
		SaveConfig: func(string, *config.Config) error { return nil },
		Restart:    func(context.Context) error { return nil },
		Version:    "e2e",
		// TunnelMarker is the per-process secret the relay-tunnelled enroll forward
		// must carry so enrollAllowed takes the PIN-gated tunneled branch (not the
		// LAN-source branch). Real value; the host wrapper below stamps it.
		TunnelMarker:       "e2e-tunnel-marker",
		SiteID:             siteID,
		RelayBaseURL:       relayURL,
		InstanceSigner:     identity,
		SiteIdentityPubHex: identity.PublicKeyHex(),
		OwnerAccessRPID:    "home.test",
		OwnerAccessOrigins: []string{"https://home.test"},
	}
	piSrv := api.New(deps)

	// Pi LAN listener (loopback) for the direct in-process flow + the tunnel host
	// reverse target. The PIN mint is driven over the handler with a spoofed LAN
	// RemoteAddr (loopback is rejected by isLANClientSource), so we don't strictly
	// need this socket for the mint — but the tunnel host needs a handler.
	piHandler := piSrv.Handler()

	// ---- Register the site with the relay (signed /me/register) ----
	hostID := "owner-e2e-host"
	registerSite(t, relayURL, siteID, hostID, identity)

	// ---- Tunnel host serving the REAL Pi enroll handler, marker-stamped ----
	// This is the production wiring the enroll-forward needs: drain the relay queue
	// for hostID and serve the Pi handler with X-FTW-Tunnel stamped so the Pi's
	// enrollAllowed tunneled branch (PIN-gated) is taken.
	markerStamped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-FTW-Tunnel", "e2e-tunnel-marker")
		piHandler.ServeHTTP(w, r)
	})
	host := tunnel.NewHost(relayURL, hostID, markerStamped)
	host.PollTimeout = time.Second
	host.SetPollSecret(registerPollSecret)
	hostCtx, hostCancel := context.WithCancel(context.Background())
	defer hostCancel()
	go host.Run(hostCtx)

	// ---- 1. Mint PIN + bootstrap_id on the (simulated-LAN) Pi ----
	// isLANClientSource rejects loopback, so spoof a genuine private-range source.
	pinReq := httptest.NewRequest(http.MethodGet, "/api/owner-access/enroll-pin", nil)
	pinReq.RemoteAddr = "192.168.7.42:53124"
	pinRec := httptest.NewRecorder()
	piHandler.ServeHTTP(pinRec, pinReq)
	if pinRec.Code != http.StatusOK {
		t.Fatalf("enroll-pin: got %d body=%q (want 200; LAN source must mint)", pinRec.Code, pinRec.Body.String())
	}
	var minted struct {
		Pin         string `json:"pin"`
		BootstrapID string `json:"bootstrap_id"`
		ExpiresInS  int    `json:"expires_in_s"`
	}
	if err := json.Unmarshal(pinRec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("decode enroll-pin: %v", err)
	}
	if len(minted.Pin) != 6 || minted.BootstrapID == "" {
		t.Fatalf("enroll-pin returned pin=%q bootstrap_id=%q", minted.Pin, minted.BootstrapID)
	}
	// The bootstrap_id must be high-entropy (>=32 bytes, base64url-no-pad) — NOT the
	// 6-digit PIN. Decode and length-check so a regression to a low-entropy handle
	// fails here.
	if bidBytes, err := base64.RawURLEncoding.DecodeString(minted.BootstrapID); err != nil || len(bidBytes) < 32 {
		t.Fatalf("bootstrap_id is not >=32-byte base64url: %q (err=%v len=%d)", minted.BootstrapID, err, len(bidBytes))
	}

	// claim_key the browser would derive from the URL #fragment.
	ckBytes := sha256.Sum256([]byte(minted.BootstrapID))
	claimKey := hex.EncodeToString(ckBytes[:])
	// The PIN must never derive the claim_key (the whole point of the rework).
	pinCK := sha256.Sum256([]byte(minted.Pin))
	if claimKey == hex.EncodeToString(pinCK[:]) {
		t.Fatal("claim_key derived from the PIN — must be hex(sha256(bootstrap_id))")
	}

	// ---- 2. Claim the Pi's self-published descriptor through the relay ----
	// The self-publish is a goroutine fired by enroll-pin; poll the relay claim.
	var descJSON []byte
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		body, _ := json.Marshal(map[string]string{"claim_key": claimKey})
		req, _ := http.NewRequest(http.MethodPost, relayURL+"/bootstrap/claim", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var out struct {
				SiteID     string `json:"site_id"`
				Descriptor string `json:"descriptor"`
			}
			if err := json.Unmarshal(b, &out); err != nil {
				t.Fatalf("decode claim: %v (%s)", err, b)
			}
			if out.SiteID != siteID {
				t.Fatalf("claim site_id=%q want %q", out.SiteID, siteID)
			}
			descJSON = []byte(out.Descriptor)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if descJSON == nil {
		t.Fatal("timed out claiming the Pi's self-published bootstrap descriptor from the relay")
	}

	// ---- 3. Verify the INNER descriptor sig exactly as the browser verifyEntry would ----
	var desc struct {
		SiteID   string `json:"site_id"`
		PiPubkey string `json:"pi_pubkey"`
		Label    string `json:"label"`
		Sig      string `json:"sig"`
	}
	if err := json.Unmarshal(descJSON, &desc); err != nil {
		t.Fatalf("descriptor JSON: %v (%s)", err, descJSON)
	}
	if desc.SiteID != siteID || desc.Label != siteName || desc.PiPubkey != identity.PublicKeyHex() {
		t.Fatalf("descriptor fields wrong: %+v", desc)
	}
	innerMsg := "ftw-instance:v1:" + desc.SiteID + ":" + desc.PiPubkey + ":" + desc.Label
	if !verifyEntryGo(desc.PiPubkey, innerMsg, desc.Sig) {
		t.Fatalf("INNER descriptor sig (base64url) does not verify (browser verifyEntry would reject); sig=%q", desc.Sig)
	}

	// ---- 4. C2 fail-closed: a no-claim_key enroll forward is REFUSED at the relay ----
	noKey, _ := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/start?pin="+minted.Pin)
	if noKey != http.StatusForbidden {
		t.Fatalf("no-claim_key enroll/start: got %d, want 403 (C2 fail-closed at relay)", noKey)
	}

	// ---- 5. C2 fail-closed: claim_key but NO pin is REFUSED at the Pi ----
	// The relay gate opens on the claim_key, forwards to the real Pi, whose
	// enrollAllowed tunneled branch rejects a missing/blank PIN. Must NOT be 200.
	noPin, _ := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/start?claim_key="+claimKey)
	if noPin == http.StatusOK {
		t.Fatalf("claim_key-but-no-pin enroll/start: got 200 — the Pi must refuse a missing PIN")
	}

	// ---- 6. Happy path: claim_key + correct PIN reaches the real Pi enroll handler ----
	code, body := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/start?claim_key="+claimKey+"&pin="+minted.Pin)
	if code != http.StatusOK {
		t.Fatalf("enroll/start (claim_key+pin): got %d body=%q, want 200 (challenge from the real Pi)", code, body)
	}
	if !strings.Contains(body, "ceremony_token") || !strings.Contains(body, "challenge") {
		t.Fatalf("enroll/start body missing WebAuthn challenge: %q", body)
	}

	// ---- 7. The forwarded enroll/finish must CARRY ceremony_token to the Pi ----
	// The browser sends ceremony_token + name ONLY in the query string of the finish
	// POST (enroll.html), never in the body. The relay must preserve the whole query
	// (minus the relay-private claim_key) when forwarding — a regression to a
	// hardcoded "?pin=" silently drops ceremony_token and the Pi 400s
	// "ceremony_token required", which would make multi-tenant enroll uncompletable.
	//
	// We assert the discriminating status: a finish forwarded WITH a (bogus)
	// ceremony_token reaches the real Pi handler and is rejected 403 "unknown or
	// expired ceremony_token" — NOT 400 "ceremony_token required". A 400 here would
	// prove the relay dropped the param. (Driving a fully valid WebAuthn finish needs
	// a software authenticator the browser owns; the security-critical relay->Pi
	// param contract is fully exercised by this status discrimination.)
	finishQ := url.Values{}
	finishQ.Set("ceremony_token", "bogus-but-present-token")
	finishQ.Set("name", "Test Device & Co") // space + '&' must round-trip un-mangled
	finishQ.Set("claim_key", claimKey)
	finishQ.Set("pin", minted.Pin)
	fcode, fbody := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/finish?"+finishQ.Encode())
	if fcode == http.StatusBadRequest {
		t.Fatalf("enroll/finish forwarded WITHOUT ceremony_token (relay dropped the query): got 400 %q", fbody)
	}
	if fcode != http.StatusForbidden {
		t.Fatalf("enroll/finish (bogus ceremony_token): got %d body=%q, want 403 (token reached the Pi, rejected as unknown)", fcode, fbody)
	}
	if !strings.Contains(fbody, "ceremony_token") {
		t.Fatalf("enroll/finish 403 body did not mention ceremony_token: %q", fbody)
	}
}

// registerPollSecret is captured by registerSite so the tunnel host can present
// the relay-minted token on its polls.
var registerPollSecret string

// registerSite POSTs a signed /me/register so the relay pins the site key (needed
// by bootstrapPut/PublicKeyForSite) and routes the host queue. It captures the
// returned poll secret into registerPollSecret.
func registerSite(t *testing.T, relayURL, siteID, hostID string, signer *nova.Identity) {
	t.Helper()
	tsMs := time.Now().UnixMilli()
	sig, err := signer.SignRawHex(tunnel.MeRegisterSigningString(siteID, hostID, tsMs))
	if err != nil {
		t.Fatalf("sign register: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"site_id":    siteID,
		"host_id":    hostID,
		"public_key": signer.PublicKeyHex(),
		"ts_ms":      tsMs,
		"sig":        sig,
	})
	resp, err := http.Post(relayURL+"/me/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status %d body %q", resp.StatusCode, b)
	}
	var out struct {
		PollSecret string `json:"poll_secret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	registerPollSecret = out.PollSecret
}

// homeEnrollPost issues a POST to the relay's home host (home.test) for an enroll
// path, returning the status + body. Mirrors the browser POSTing the enroll RPC
// at the home origin.
func homeEnrollPost(t *testing.T, relayURL, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, relayURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "home.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// verifyEntryGo mirrors web/owner-access/instance-sync.js verifyEntry: a base64url
// (no padding) raw r||s ECDSA-P256 / SHA-256 sig over msg, against an uncompressed
// X||Y device pubkey (128 hex chars). If this rejects a Pi-signed descriptor, the
// browser would too.
func verifyEntryGo(pubKeyHex, msg, sigB64URL string) bool {
	pb, err := hex.DecodeString(pubKeyHex)
	if err != nil || len(pb) != 64 {
		return false
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(pb[:32]),
		Y:     new(big.Int).SetBytes(pb[32:]),
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64URL)
	if err != nil || len(sig) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(msg))
	return ecdsa.Verify(pub, h[:], r, s)
}
