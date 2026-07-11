package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
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
	"github.com/fxamacker/cbor/v2"
)

// TestBootstrapOnboardingThroughRelay is the full-stack e2e for the multi-tenant
// onboarding bootstrap on a HIGH-ENTROPY bootstrap_id, in its REVISION-2 end state
// (ceremony-bound possession proof + single-use-before-side-effects). It wires the
// REAL ftw-relay binary in -multi-tenant mode to a REAL in-process Pi (api.Server +
// its self-sovereign nova.Identity) and walks the whole courier:
//
//	mint PIN + bootstrap_id on the (simulated-LAN) Pi
//	  -> Pi self-publishes its signed descriptor keyed by hex(sha256(bootstrap_id)) + ts_ms
//	  -> a fresh client computes claim_key from the bootstrap_id, claims the
//	     descriptor back through the relay, and verifies its INNER sig exactly as the
//	     browser's verifyEntry would (base64url r||s ECDSA-P256 over the instance string)
//	  -> the client enrolls via the relay forward (?claim_key gate + ?pin Pi factor +
//	     ?bootstrap_proof possession proof), the relay RESERVES single-use before
//	     forwarding the finish, and the REAL Pi enroll handler validates the PIN, the
//	     ceremony-bound proof, and the zero-device window before persisting.
//
// REVISION-2 contract exercised end to end through the real relay->Pi path:
//   - HAPPY PATH: a full software-attested (none-attestation) enroll/start +
//     enroll/finish with a CORRECT bootstrap_proof = hex(HMAC-SHA256(bootstrap_id,
//     ceremony_token)) -> 200, device persisted on the real Pi.
//   - PROOF GATE (closes the relay-visible-PIN HIGH): a finish with a WRONG proof,
//     and a finish with a MISSING proof, are both rejected 403 by the Pi before any
//     device is saved.
//   - RELAY RESERVATION (closes the concurrent-double-finish BLOCKER): two
//     concurrent finishes (distinct ceremony_tokens, each with a valid proof) through
//     the relay yield EXACTLY one 200 and one 403 — the loser loses the test-and-set
//     reservation before its enroll ever reaches the Pi.
//   - PI ZERO-DEVICE RECHECK (source-of-truth backstop for the BLOCKER): once a
//     device exists, the Pi refuses to mint another first-setup QR/PIN; the
//     lower finish-time LoadTrustedDevices recheck remains covered in API tests.
//   - C2 fail-closed (unchanged): a no-claim_key forward is refused at the relay
//     (403); a claim_key-but-no-PIN forward is refused at the Pi.
//
// Two facts shape what is asserted here (documented, not papered over):
//   - mintEnrollPin requires a genuine PRIVATE-RANGE LAN source (isLANClientSource
//     rejects loopback). We therefore drive the PIN-mint over the in-process handler
//     with a spoofed private-range RemoteAddr — that is the real Pi code path,
//     including the synchronous self-publish to the relay. This keeps the test
//     deterministic in CI (no dependency on a real LAN interface, which `make verify`
//     runs without -short would otherwise make flaky).
//   - The production enroll-forward HOST that drains the relay queue and stamps
//     X-FTW-Tunnel is cmd/forty-two-watts/owner_relay_register.go's
//     staticAssetHandler (the F4 component). It lives in `package main`, which an
//     external test package cannot import, and the full binary cannot be booted
//     in-process (monolithic main with os.Exit + the LAN-source PIN-mint constraint
//     above). This test therefore drives a tunnel host that mirrors that handler's
//     production gating BYTE FOR BYTE — the enrollForwardPaths allowlist, POST-only,
//     X-FTW-Tunnel marker stamp, and Set-Cookie strip — against the REAL Pi
//     api.Server. The genuine staticAssetHandler is unit-tested directly in
//     owner_relay_register_test.go (TestEnrollForwardHost_*). See the
//     DONE_WITH_CONCERNS note in the task report.
func TestBootstrapOnboardingThroughRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e: skipped in short mode")
	}
	repo := repoRoot(t)
	relayBin := buildBinary(t, repo, "ftw-relay")

	const siteID = "site:e2e"
	const siteName = "e2e"
	const rpID = "home.test"
	const origin = "https://home.test"
	const tunnelMarker = "e2e-tunnel-marker"

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
	cfg.RemoteAccess = &config.RemoteAccess{Enabled: true}
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
		// LAN-source branch) AND handleOwnerEnrollFinish takes the proof + zero-device
		// recheck branch. Real value; the host wrapper below stamps it exactly as the
		// production staticAssetHandler does.
		TunnelMarker:       tunnelMarker,
		SiteID:             siteID,
		RelayBaseURL:       relayURL,
		InstanceSigner:     identity,
		SiteIdentityPubHex: identity.PublicKeyHex(),
		OwnerAccessRPID:    rpID,
		OwnerAccessOrigins: []string{origin},
	}
	piSrv := api.New(deps)
	piHandler := piSrv.Handler()

	// ---- Register the site with the relay (signed /me/register) ----
	hostID := "owner-e2e-host"
	registerSite(t, relayURL, siteID, hostID, identity)

	// ---- Tunnel host serving the REAL Pi enroll handler, gated exactly as the
	// production staticAssetHandler (owner_relay_register.go) does ----
	host := tunnel.NewHost(relayURL, hostID, newProdEnrollForwardHandler(piHandler, tunnelMarker))
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
	// The self-publish completed before enroll-pin returned; claim the descriptor
	// through the relay just like the browser does.
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
	noKey, _ := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/start?pin="+minted.Pin, nil)
	if noKey != http.StatusForbidden {
		t.Fatalf("no-claim_key enroll/start: got %d, want 403 (C2 fail-closed at relay)", noKey)
	}

	// ---- 5. C2 fail-closed: claim_key but NO pin is REFUSED at the Pi ----
	// The relay gate opens on the claim_key, forwards to the real Pi, whose
	// enrollAllowed tunneled branch rejects a missing/blank PIN. Must NOT be 200.
	noPin, _ := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/start?claim_key="+claimKey, nil)
	if noPin == http.StatusOK {
		t.Fatalf("claim_key-but-no-pin enroll/start: got 200 — the Pi must refuse a missing PIN")
	}

	// authQ is the per-RPC query the browser carries on BOTH start and finish: the
	// relay-private claim_key (stripped before forwarding) plus the PIN (forwarded
	// untouched to the Pi).
	authQ := func() url.Values {
		q := url.Values{}
		q.Set("claim_key", claimKey)
		q.Set("pin", minted.Pin)
		return q
	}

	// The relay throttles enroll-forwards per source IP (token bucket: capacity 8,
	// refill 2/s) on the un-spoofable loopback RemoteAddr. Every phase below pays
	// from the SAME bucket, so we pace before each group of forwards to let it refill
	// — pacing keeps the security assertions deterministic instead of racing a 429.
	pace := func(tokens int) { time.Sleep(time.Duration(tokens) * 600 * time.Millisecond) }

	// ---- 6. PROOF GATE: a tunneled finish with a WRONG bootstrap_proof is 403 ----
	// (Closes the relay-visible-PIN HIGH.) A relay that only holds sha256(bootstrap_id)
	// cannot compute hex(HMAC-SHA256(bootstrap_id, ceremony_token)); a wrong-key HMAC
	// is what such a relay could at best produce — the Pi rejects it before persisting.
	{
		pace(2) // start + finish
		code, body := softwareEnrollThroughRelay(t, relayURL, rpID, origin, authQ(),
			func(ceremonyToken string, finishBody []byte) string {
				return jsHMACProofE2E("not-the-real-bootstrap-id", ceremonyToken, finishBody)
			})
		if code != http.StatusForbidden {
			t.Fatalf("tunneled finish with WRONG proof: got %d body=%q, want 403", code, body)
		}
		assertNoDevice(t, st, "after a wrong-proof finish no device may be saved")
	}

	// ---- 7. PROOF GATE: a tunneled finish with a MISSING bootstrap_proof is 403 ----
	{
		pace(2) // start + finish
		code, body := softwareEnrollThroughRelay(t, relayURL, rpID, origin, authQ(),
			func(string, []byte) string { return "" }, // no bootstrap_proof param
		)
		if code != http.StatusForbidden {
			t.Fatalf("tunneled finish with MISSING proof: got %d body=%q, want 403", code, body)
		}
		assertNoDevice(t, st, "after a missing-proof finish no device may be saved")
	}

	// ---- 7b. PROOF GATE (closes the device_pubkey-swap HIGH): a MITM relay that
	// swaps the top-level device_pubkey in the forwarded finish body — AFTER the
	// browser computed a VALID proof over the honest body — is refused 403 by the Pi.
	// The proof binds a hash of the exact body, so the relay would have to recompute
	// the HMAC (which needs the raw bootstrap_id it never holds). The honest path
	// (step 8) still 200s, proving this isn't a blanket rejection.
	{
		pace(2) // start + finish
		attackerKey := freshCanonicalDeviceKeyHexE2E(t)
		code, body := softwareEnrollThroughRelayTamper(t, relayURL, rpID, origin, authQ(),
			func(ceremonyToken string, finishBody []byte) string {
				// Honest browser: valid proof over the body it actually authored.
				return jsHMACProofE2E(minted.BootstrapID, ceremonyToken, finishBody)
			},
			func(finishBody []byte) []byte {
				// MITM relay: inject its OWN device_pubkey into the forwarded body.
				var m map[string]any
				if err := json.Unmarshal(finishBody, &m); err != nil {
					t.Fatalf("tamper: unmarshal honest finish body: %v", err)
				}
				m["device_pubkey"] = attackerKey
				out, err := json.Marshal(m)
				if err != nil {
					t.Fatalf("tamper: marshal swapped body: %v", err)
				}
				return out
			})
		if code != http.StatusForbidden {
			t.Fatalf("tunneled finish with a device_pubkey SWAPPED by a MITM relay: got %d body=%q, want 403", code, body)
		}
		assertNoDevice(t, st, "after a device_pubkey-swap finish no device may be saved")
	}

	// ---- 8. RELAY RESERVATION + HAPPY PATH: concurrent double-finish ----
	// (Closes the concurrent-double-finish BLOCKER.) Two distinct software-attested
	// ceremonies are STARTED sequentially (each its own ceremony_token + credential),
	// then their two finishes — each with a CORRECT proof — fire CONCURRENTLY through
	// the relay. The relay's Reserve test-and-set lets exactly one finish reach the Pi
	// (-> 200, device persisted, bootstrap Burned); the other loses the latch and is
	// refused 403 BEFORE its enroll could touch the Pi. We assert EXACTLY one 200 and
	// one 403 — and that the 200 is a genuine, fully software-attested enroll (the
	// happy path) by confirming a device landed.
	{
		const n = 2
		// Start both ceremonies sequentially (pace each start) so the only thing
		// racing is the pair of finishes — the surface the relay reservation guards.
		ceremonyTokens := make([]string, n)
		finishBodies := make([]string, n)
		for i := 0; i < n; i++ {
			pace(1)
			startQ := cloneValues(authQ())
			code, body := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/start?"+startQ.Encode(), nil)
			if code != http.StatusOK {
				t.Fatalf("concurrent-prep enroll/start #%d: got %d body=%q", i, code, body)
			}
			tok, challenge := parseStart(t, body)
			ceremonyTokens[i] = tok
			finishBodies[i] = fabricateRegistrationResponse(t, rpID, origin, challenge)
		}
		// Refill the bucket so both concurrent finishes have a token at the same
		// instant — otherwise a 429 (not the reservation 403) could mask the result.
		pace(4)

		type res struct {
			code int
			body string
		}
		results := make([]res, n)
		var wg sync.WaitGroup
		var startGate sync.WaitGroup
		startGate.Add(1)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				fq := cloneValues(authQ())
				fq.Set("ceremony_token", ceremonyTokens[i])
				fq.Set("name", "Test Device & Co")
				// The proof binds the EXACT body this goroutine POSTs (finishBodies[i]).
				fq.Set("bootstrap_proof", jsHMACProofE2E(minted.BootstrapID, ceremonyTokens[i], []byte(finishBodies[i])))
				startGate.Wait() // release both finishes as close to simultaneously as possible
				c, b := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/finish?"+fq.Encode(), strings.NewReader(finishBodies[i]))
				results[i] = res{c, b}
			}(i)
		}
		startGate.Done()
		wg.Wait()
		var got200, got403, other int
		var loserBody string
		for _, r := range results {
			switch r.code {
			case http.StatusOK:
				got200++
			case http.StatusForbidden:
				got403++
				loserBody = r.body
			default:
				other++
				t.Logf("concurrent finish unexpected status %d body=%q", r.code, r.body)
			}
		}
		if got200 != 1 || got403 != 1 || other != 0 {
			t.Fatalf("concurrent double-finish: got %d×200 %d×403 %d×other, want exactly 1×200 + 1×403 (relay single-use reservation)", got200, got403, other)
		}
		// Document which fail-closed layer caught the loser. The relay's Reserve
		// test-and-set is the primary guard ("no live bootstrap for this claim_key");
		// the Pi's finish-time zero-device recheck is the source-of-truth backstop
		// ("enrollment window closed"). Either is a valid 403, so we only LOG which
		// one fired here rather than over-constrain a legitimately-racing outcome.
		t.Logf("concurrent double-finish loser 403 body=%q (relay reservation or Pi recheck — both fail-closed)", strings.TrimSpace(loserBody))
		// HAPPY PATH proof: the single 200 was a genuine software-attested enroll —
		// a trusted device must now exist on the real Pi.
		devs, err := st.LoadTrustedDevices()
		if err != nil {
			t.Fatalf("load trusted devices after happy-path enroll: %v", err)
		}
		if len(devs) != 1 {
			t.Fatalf("after the winning software-attested finish want exactly 1 enrolled device, got %d", len(devs))
		}
	}

	// ---- 9. CLOSED FIRST-SETUP WINDOW ----
	// A device now exists, and the relay BURNED the bootstrap on the winning finish.
	// The Pi must not mint or re-publish another first-setup QR/PIN while a
	// passkey exists; adding more passkeys is authenticated owner management.
	{
		pinReq2 := httptest.NewRequest(http.MethodGet, "/api/owner-access/enroll-pin", nil)
		pinReq2.RemoteAddr = "192.168.7.42:53200"
		pinRec2 := httptest.NewRecorder()
		piHandler.ServeHTTP(pinRec2, pinReq2)
		if pinRec2.Code != http.StatusConflict {
			t.Fatalf("re-mint enroll-pin after device exists: got %d body=%q, want 409 closed first-setup window", pinRec2.Code, pinRec2.Body.String())
		}
		// Still exactly one device — the closed first-setup window did not enroll
		// a second permanent owner credential.
		devs, err := st.LoadTrustedDevices()
		if err != nil {
			t.Fatalf("load trusted devices after recheck: %v", err)
		}
		if len(devs) != 1 {
			t.Fatalf("closed first-setup window failed: want 1 device, got %d", len(devs))
		}
	}
}

// newProdEnrollForwardHandler returns a tunnel-host handler that mirrors the
// production staticAssetHandler (cmd/forty-two-watts/owner_relay_register.go) under
// -multi-tenant, BYTE FOR BYTE for the security-relevant gating: only POST of the
// two enroll paths is forwarded to the Pi with X-FTW-Tunnel stamped (so the Pi's
// isTunneled gate fires — PIN + possession-proof + zero-device recheck +
// owner-cookie suppression); the Set-Cookie response header is stripped; every other
// /api/* path is 403 and every non-GET method is 405. The genuine staticAssetHandler
// is unit-tested in owner_relay_register_test.go — this faithful mirror is used here
// only because `package main` cannot be imported by an external test package.
func newProdEnrollForwardHandler(piHandler http.Handler, tunnelMarker string) http.Handler {
	enrollPaths := map[string]struct{}{
		"/api/owner-access/enroll/start":  {},
		"/api/owner-access/enroll/finish": {},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if _, ok := enrollPaths[r.URL.Path]; ok {
				r.Header.Set("X-FTW-Tunnel", tunnelMarker)
				piHandler.ServeHTTP(&e2eStripSetCookie{ResponseWriter: w}, r)
				return
			}
		}
		if r.Method != http.MethodGet {
			http.Error(w, "owner API is P2P-only", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/identity" {
			http.Error(w, "owner API is P2P-only", http.StatusForbidden)
			return
		}
		r.Header.Del("Cookie")
		piHandler.ServeHTTP(&e2eStripSetCookie{ResponseWriter: w}, r)
	})
}

// e2eStripSetCookie drops any Set-Cookie the Pi emits, mirroring the production
// stripSetCookieWriter so the owner session cookie can never traverse the relay.
type e2eStripSetCookie struct {
	http.ResponseWriter
	wrote bool
}

func (w *e2eStripSetCookie) WriteHeader(code int) {
	if !w.wrote {
		w.Header().Del("Set-Cookie")
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *e2eStripSetCookie) Write(b []byte) (int, error) {
	if !w.wrote {
		w.Header().Del("Set-Cookie")
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
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
// path, returning the status + body. Mirrors the browser POSTing the enroll RPC at
// the home origin. body may be nil (no request body).
func homeEnrollPost(t *testing.T, relayURL, path string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, relayURL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "home.test"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

// softwareEnrollThroughRelay runs a FULL software-attested (none-attestation)
// enroll/start + enroll/finish through the relay's home host (the real
// bootstrapEnrollForward) into the real Pi. authQ is the per-RPC query (claim_key +
// pin) carried on BOTH legs exactly as the browser sends them. proofFor is called
// with the ceremony_token returned by enroll/start AND the EXACT finish body bytes
// the browser POSTs; its non-empty return is appended to the finish query as
// ?bootstrap_proof=<hex> (the browser sends the proof only on finish, computed over
// the body it sends). An empty return omits the proof param entirely (the
// missing-proof case). Returns the finish status + body.
func softwareEnrollThroughRelay(t *testing.T, relayURL, rpID, origin string, authQ url.Values, proofFor func(ceremonyToken string, body []byte) string) (int, string) {
	t.Helper()
	return softwareEnrollThroughRelayTamper(t, relayURL, rpID, origin, authQ, proofFor, nil)
}

// softwareEnrollThroughRelayTamper is softwareEnrollThroughRelay with a tamper hook
// that models a MITM relay rewriting the forwarded finish body AFTER the browser
// computed the proof over the honest body. The body-bound proof must make the Pi
// reject the tampered forward.
func softwareEnrollThroughRelayTamper(t *testing.T, relayURL, rpID, origin string, authQ url.Values, proofFor func(ceremonyToken string, body []byte) string, tamper func(body []byte) []byte) (int, string) {
	t.Helper()

	// --- enroll/start through the relay (carries claim_key + pin) ---
	startQ := cloneValues(authQ)
	startCode, startBody := homeEnrollPost(t, relayURL, "/api/owner-access/enroll/start?"+startQ.Encode(), nil)
	if startCode != http.StatusOK {
		// A failed start (e.g. dead bootstrap) is reported up so the caller's assert
		// reflects the real status. We synthesise the same shape as a finish failure.
		return startCode, startBody
	}
	ceremonyToken, challenge := parseStart(t, startBody)

	// --- fabricate the none-attestation registration response ---
	finishJSON := fabricateRegistrationResponse(t, rpID, origin, challenge)

	// --- enroll/finish through the relay (claim_key + pin + ceremony_token + name + proof) ---
	finishQ := cloneValues(authQ)
	finishQ.Set("ceremony_token", ceremonyToken)
	finishQ.Set("name", "Test Device & Co") // space + '&' must round-trip un-mangled
	// The browser computes the proof over the EXACT body it POSTs (the honest body).
	if proof := proofFor(ceremonyToken, []byte(finishJSON)); proof != "" {
		finishQ.Set("bootstrap_proof", proof)
	}
	// A MITM relay swaps device_pubkey in the forwarded body AFTER the proof was
	// computed: the bytes the Pi receives differ from the proven body, so it 403s.
	sendJSON := finishJSON
	if tamper != nil {
		sendJSON = string(tamper([]byte(finishJSON)))
	}
	return homeEnrollPost(t, relayURL, "/api/owner-access/enroll/finish?"+finishQ.Encode(), strings.NewReader(sendJSON))
}

// softwareEnrollDirectTunneled runs a full software-attested enroll/start +
// enroll/finish DIRECTLY against the Pi handler with the production X-FTW-Tunnel
// marker stamped (the exact isTunneled branch the relay forward takes). Used for the
// post-enrollment Pi zero-device recheck, where the relay can no longer supply a
// live bootstrap. Carries pin + a CORRECT bootstrap_proof. Returns finish status+body.
func softwareEnrollDirectTunneled(t *testing.T, piHandler http.Handler, rpID, origin, tunnelMarker, pin, bootstrapID string) (int, string) {
	t.Helper()
	startReq := httptest.NewRequest(http.MethodPost, "/api/owner-access/enroll/start?pin="+pin, nil)
	startReq.Host = rpID
	startReq.RemoteAddr = "192.168.7.42:40000"
	startReq.Header.Set("X-FTW-Tunnel", tunnelMarker)
	startRec := httptest.NewRecorder()
	piHandler.ServeHTTP(startRec, startReq)
	if startRec.Code != http.StatusOK {
		return startRec.Code, startRec.Body.String()
	}
	ceremonyToken, challenge := parseStart(t, startRec.Body.String())
	finishJSON := fabricateRegistrationResponse(t, rpID, origin, challenge)
	finishURL := "/api/owner-access/enroll/finish?pin=" + pin +
		"&ceremony_token=" + ceremonyToken +
		"&bootstrap_proof=" + jsHMACProofE2E(bootstrapID, ceremonyToken, []byte(finishJSON))
	finishReq := httptest.NewRequest(http.MethodPost, finishURL, strings.NewReader(finishJSON))
	finishReq.Host = rpID
	finishReq.RemoteAddr = "192.168.7.42:40001"
	finishReq.Header.Set("Content-Type", "application/json")
	finishReq.Header.Set("X-FTW-Tunnel", tunnelMarker)
	finishRec := httptest.NewRecorder()
	piHandler.ServeHTTP(finishRec, finishReq)
	return finishRec.Code, finishRec.Body.String()
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vs := range v {
		for _, s := range vs {
			out.Add(k, s)
		}
	}
	return out
}

// parseStart pulls the ceremony_token + WebAuthn challenge (base64url-no-pad) out of
// an enroll/start response body.
func parseStart(t *testing.T, body string) (ceremonyToken, challenge string) {
	t.Helper()
	var start struct {
		CeremonyToken string `json:"ceremony_token"`
		Options       struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal([]byte(body), &start); err != nil {
		t.Fatalf("decode enroll/start: %v body=%q", err, body)
	}
	if start.CeremonyToken == "" || start.Options.PublicKey.Challenge == "" {
		t.Fatalf("enroll/start missing ceremony_token or challenge: %q", body)
	}
	return start.CeremonyToken, start.Options.PublicKey.Challenge
}

// fabricateRegistrationResponse builds a valid none-attestation WebAuthn
// registration response over a fresh P-256 credential, mirroring the software
// authenticator in go/internal/api/api_owner_enroll_cookie_test.go. Each call uses a
// fresh random credential ID so concurrent ceremonies don't collide on the
// WithExclusions list. rpID/origin must match the Pi's WebAuthn config.
func fabricateRegistrationResponse(t *testing.T, rpID, origin, challenge string) string {
	t.Helper()
	clientData := map[string]any{
		"type":      "webauthn.create",
		"challenge": challenge,
		"origin":    origin,
	}
	clientDataJSON, err := json.Marshal(clientData)
	if err != nil {
		t.Fatalf("marshal clientData: %v", err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen credential key: %v", err)
	}
	xb := make([]byte, 32)
	yb := make([]byte, 32)
	priv.PublicKey.X.FillBytes(xb)
	priv.PublicKey.Y.FillBytes(yb)
	em, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		t.Fatalf("cbor enc mode: %v", err)
	}
	coseKey, err := em.Marshal(map[int]any{
		1:  2,  // kty: EC2
		3:  -7, // alg: ES256
		-1: 1,  // crv: P-256
		-2: xb, // x
		-3: yb, // y
	})
	if err != nil {
		t.Fatalf("marshal cose key: %v", err)
	}

	// Fresh random credential ID per ceremony (concurrent enrolls must not collide).
	credID := make([]byte, 20)
	if _, err := rand.Read(credID); err != nil {
		t.Fatalf("rand cred id: %v", err)
	}

	rpHash := sha256.Sum256([]byte(rpID))
	authData := make([]byte, 0, 37+16+2+len(credID)+len(coseKey))
	authData = append(authData, rpHash[:]...)
	authData = append(authData, 0x01|0x40) // flags: UP | AT
	authData = append(authData, 0, 0, 0, 0)
	authData = append(authData, make([]byte, 16)...) // AAGUID = all-zero
	authData = append(authData, byte(len(credID)>>8), byte(len(credID)))
	authData = append(authData, credID...)
	authData = append(authData, coseKey...)

	attObj, err := em.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": authData,
	})
	if err != nil {
		t.Fatalf("marshal attestationObject: %v", err)
	}

	credIDB64 := base64.RawURLEncoding.EncodeToString(credID)
	finishBody := map[string]any{
		"id":    credIDB64,
		"rawId": credIDB64,
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    base64.RawURLEncoding.EncodeToString(clientDataJSON),
			"attestationObject": base64.RawURLEncoding.EncodeToString(attObj),
		},
	}
	finishJSON, err := json.Marshal(finishBody)
	if err != nil {
		t.Fatalf("marshal finish body: %v", err)
	}
	return string(finishJSON)
}

// jsHMACProofE2E recomputes the browser's ceremony-bound, BODY-bound possession
// proof so the e2e verifies the real Pi validator against the SAME construction the
// browser uses: hex(HMAC-SHA256(key=utf8(bootstrap_id),
// msg=utf8(ceremony_token + "|" + hex(sha256(body))))). Binding the body hash means
// a MITM relay cannot swap device_pubkey (or the attestation, or the name) in the
// forwarded finish body without breaking the proof. Hand-rolled (not calling the
// SUT) so a drift from the Pi/browser construction is caught here. body MUST be the
// EXACT finish body bytes POSTed.
func jsHMACProofE2E(bootstrapID, ceremonyToken string, body []byte) string {
	sum := sha256.Sum256(body)
	msg := ceremonyToken + "|" + hex.EncodeToString(sum[:])
	mac := hmac.New(sha256.New, []byte(bootstrapID))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// freshCanonicalDeviceKeyHexE2E returns a fresh, canonical-form (lowercase, on-curve)
// 128-hex uncompressed P-256 public key — the exact shape the Pi's canonicalDevicePubkey
// accepts and would store. Models the key a MITM relay would substitute on the forward.
func freshCanonicalDeviceKeyHexE2E(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen device key: %v", err)
	}
	xb := make([]byte, 32)
	yb := make([]byte, 32)
	priv.PublicKey.X.FillBytes(xb)
	priv.PublicKey.Y.FillBytes(yb)
	return hex.EncodeToString(xb) + hex.EncodeToString(yb)
}

// assertNoDevice fails if any trusted device is enrolled — used after a rejected
// finish to prove no device was persisted.
func assertNoDevice(t *testing.T, st *state.Store, msg string) {
	t.Helper()
	devs, err := st.LoadTrustedDevices()
	if err != nil {
		t.Fatalf("load trusted devices (%s): %v", msg, err)
	}
	if len(devs) != 0 {
		t.Fatalf("%s: got %d devices", msg, len(devs))
	}
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
