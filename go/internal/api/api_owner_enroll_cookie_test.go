package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
	"github.com/fxamacker/cbor/v2"
)

// This file drives a REAL WebAuthn enroll/start + enroll/finish ceremony with a
// software authenticator (none attestation), so it exercises the exact handler
// branch that mints the ftw_owner session cookie. The security contract under
// test (Task R4 step 4): the owner session cookie must NEVER be issued on a
// tunnel-forwarded (bootstrap) enroll — the relay's bootstrapEnrollForward is the
// only path that carries enroll/finish over the relay tunnel, and the directory
// seed it drives is Ed25519-write-sig-authed, so no relay-traversing session is
// needed. A DIRECT LAN enroll/finish still issues the cookie.

// softwareEnrollFinish runs a full enroll/start + enroll/finish against srv,
// fabricating a valid none-attestation registration response. extraHeaders is
// applied to BOTH requests (so the tunnel marker, if any, is present on finish).
// authQuery (e.g. "pin=123456") rides on BOTH start and finish — exactly as the
// relay's bootstrapEnrollForward forwards the PIN to the Pi on each enroll RPC.
// It returns the finish recorder so the caller can inspect Set-Cookie.
func softwareEnrollFinish(t *testing.T, srv *Server, rpID, origin string, extraHeaders map[string]string, authQuery string) *httptest.ResponseRecorder {
	t.Helper()
	return softwareEnrollFinishProof(t, srv, rpID, origin, extraHeaders, authQuery, nil)
}

// softwareEnrollFinishProof is softwareEnrollFinish with an extra proofFor
// callback: the ceremony_token isn't known until enroll/start returns, and the
// ceremony-bound bootstrap_proof now binds BOTH the ceremony_token AND a hash of
// the exact finish body bytes, so the proof can only be computed for the finish
// leg once both are known. proofFor, if non-nil, is called with the ceremony_token
// and the EXACT finish body bytes the browser POSTs; its return value (e.g.
// "bootstrap_proof=<hex>") is appended to the finish path ONLY — never to start.
// This mirrors the real browser, which sends ?bootstrap_proof only on finish.
func softwareEnrollFinishProof(t *testing.T, srv *Server, rpID, origin string, extraHeaders map[string]string, authQuery string, proofFor func(ceremonyToken string, body []byte) string) *httptest.ResponseRecorder {
	t.Helper()
	return softwareEnrollFinishTamper(t, srv, rpID, origin, extraHeaders, authQuery, proofFor, nil)
}

// softwareEnrollFinishTamper is softwareEnrollFinishProof with an extra tamper
// hook: after the honest finish body is fabricated and the proof is computed over
// it, tamper (if non-nil) rewrites the body bytes that are actually POSTed. This
// models a man-in-the-middle relay that swaps the top-level device_pubkey AFTER the
// browser computed the proof over the honest body — the body-bound proof must make
// that finish fail 403.
func softwareEnrollFinishTamper(t *testing.T, srv *Server, rpID, origin string, extraHeaders map[string]string, authQuery string, proofFor func(ceremonyToken string, body []byte) string, tamper func(body []byte) []byte) *httptest.ResponseRecorder {
	t.Helper()

	// --- enroll/start: pull the challenge out of the WebAuthn options. ---
	startPath := "/api/owner-access/enroll/start"
	if authQuery != "" {
		startPath += "?" + authQuery
	}
	startReq := httptest.NewRequest("POST", startPath, nil)
	startReq.Host = "127.0.0.1:8080"
	startReq.RemoteAddr = "192.168.1.50:1234" // genuine LAN source
	for k, v := range extraHeaders {
		startReq.Header.Set(k, v)
	}
	startRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(startRec, startReq)
	if startRec.Code != 200 {
		t.Fatalf("enroll/start: status=%d body=%q", startRec.Code, startRec.Body.String())
	}
	var start struct {
		CeremonyToken string `json:"ceremony_token"`
		Options       struct {
			PublicKey struct {
				Challenge string `json:"challenge"`
			} `json:"publicKey"`
		} `json:"options"`
	}
	if err := json.Unmarshal(startRec.Body.Bytes(), &start); err != nil {
		t.Fatalf("decode enroll/start: %v body=%q", err, startRec.Body.String())
	}
	challenge := start.Options.PublicKey.Challenge // base64url-no-pad
	if challenge == "" || start.CeremonyToken == "" {
		t.Fatalf("enroll/start missing challenge or ceremony_token: %q", startRec.Body.String())
	}

	// --- fabricate a valid none-attestation registration response. ---
	// clientDataJSON: type/challenge/origin must match what Verify expects.
	clientData := map[string]any{
		"type":      "webauthn.create",
		"challenge": challenge,
		"origin":    origin,
	}
	clientDataJSON, err := json.Marshal(clientData)
	if err != nil {
		t.Fatalf("marshal clientData: %v", err)
	}

	// A fresh P-256 credential key. The COSE public key (kty=EC2, alg=ES256,
	// crv=P-256, x, y) is embedded in the attested credential data.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen credential key: %v", err)
	}
	xb := make([]byte, 32)
	yb := make([]byte, 32)
	priv.PublicKey.X.FillBytes(xb)
	priv.PublicKey.Y.FillBytes(yb)
	// COSE_Key for EC2 P-256: {1:2, 3:-7, -1:1, -2:x, -3:y}. cbor canonical
	// encoding (the protocol parser only needs a well-formed COSE map).
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

	credID := []byte("software-credential-id-0001")

	// authenticatorData = rpIDHash(32) || flags(1) || signCount(4) ||
	//   attestedCredentialData{ aaguid(16) || credIDLen(2 BE) || credID || coseKey }
	rpHash := sha256.Sum256([]byte(rpID))
	authData := make([]byte, 0, 37+16+2+len(credID)+len(coseKey))
	authData = append(authData, rpHash[:]...)
	// flags: UP (0x01) | AT (0x40). UV not required (UserVerification=preferred).
	authData = append(authData, 0x01|0x40)
	authData = append(authData, 0, 0, 0, 0)                              // signCount = 0
	authData = append(authData, make([]byte, 16)...)                     // AAGUID = all-zero
	authData = append(authData, byte(len(credID)>>8), byte(len(credID))) // credIDLen BE
	authData = append(authData, credID...)
	authData = append(authData, coseKey...)

	// attestationObject = {fmt:"none", attStmt:{}, authData:<bytes>}
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

	// The proof binds the EXACT body the browser hashes. Compute it over the
	// honest body BEFORE any tamper rewrites the bytes actually sent — exactly as
	// the browser hashes the body string once, then POSTs it verbatim.
	path := "/api/owner-access/enroll/finish?ceremony_token=" + start.CeremonyToken
	if authQuery != "" {
		path += "&" + authQuery
	}
	if proofFor != nil {
		if fq := proofFor(start.CeremonyToken, finishJSON); fq != "" {
			path += "&" + fq
		}
	}
	// A MITM relay swaps device_pubkey AFTER the proof was computed: the body
	// actually POSTed differs from the body the proof bound, so the Pi must 403.
	sendJSON := finishJSON
	if tamper != nil {
		sendJSON = tamper(finishJSON)
	}
	finishReq := httptest.NewRequest("POST", path, strings.NewReader(string(sendJSON)))
	finishReq.Host = "127.0.0.1:8080"
	finishReq.RemoteAddr = "192.168.1.50:1234"
	finishReq.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		finishReq.Header.Set(k, v)
	}
	finishRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(finishRec, finishReq)
	return finishRec
}

func hasOwnerCookie(rec *httptest.ResponseRecorder) bool {
	for _, c := range rec.Result().Cookies() {
		if c.Name == ownerAccessCookieName && c.Value != "" {
			return true
		}
	}
	return false
}

// A DIRECT LAN enroll/finish (no tunnel marker) issues the ftw_owner session
// cookie — the on-LAN operator gets an immediate session.
func TestEnrollFinishDirectLANIssuesCookie(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	// rpID/origin must match minDeps' WebAuthn config (localhost / http://localhost).
	rec := softwareEnrollFinish(t, New(d), "localhost", "http://localhost", nil, "")
	if rec.Code != 200 {
		t.Fatalf("direct LAN enroll/finish: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !hasOwnerCookie(rec) {
		t.Fatalf("direct LAN enroll/finish must set the %s session cookie; got cookies %+v",
			ownerAccessCookieName, rec.Result().Cookies())
	}
}

// A TUNNEL-FORWARDED enroll/finish (X-FTW-Tunnel marker set, valid PIN, valid
// ceremony-bound proof) must succeed (the directory seed completes) but issue NO
// ftw_owner cookie — the owner session must never traverse the relay tunnel.
func TestEnrollFinishTunneledSuppressesCookie(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "tunnel-marker-secret"
	srv := New(d)

	// Mint a PIN + bootstrap_id (LAN) so the tunneled enroll passes both the PIN
	// gate (enrollAllowed) and the new ceremony-bound possession-proof gate.
	pin, bid, _, err := srv.ownerAccess().mintEnrollPin()
	if err != nil {
		t.Fatalf("mint pin: %v", err)
	}

	headers := map[string]string{"X-FTW-Tunnel": "tunnel-marker-secret"}
	rec := softwareEnrollFinishProof(t, srv, "localhost", "http://localhost", headers, "pin="+pin,
		func(tok string, body []byte) string { return "bootstrap_proof=" + jsHMACProof(bid, tok, body) })
	if rec.Code != 200 {
		t.Fatalf("tunneled enroll/finish: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if hasOwnerCookie(rec) {
		t.Fatalf("tunneled enroll/finish must NOT set the %s session cookie (it must never traverse the relay); got cookies %+v",
			ownerAccessCookieName, rec.Result().Cookies())
	}
}

// jsHMACProof recomputes the browser's ceremony-bound, BODY-BOUND possession proof
// so the tests verify the Go validator against the SAME construction the browser
// uses: hex(HMAC-SHA256(key=utf8(bootstrap_id),
// msg=utf8(ceremony_token + "|" + hex(sha256(body))))). Binding the body hash means
// a MITM relay cannot swap device_pubkey (or the attestation, or the name) without
// breaking the proof. If this ever drifts from bootstrapEnrollProof the interop with
// bootstrap-enroll.js breaks, so the tests deliberately hand-roll it instead of
// calling the SUT.
func jsHMACProof(bootstrapID, ceremonyToken string, body []byte) string {
	sum := sha256.Sum256(body)
	msg := ceremonyToken + "|" + hex.EncodeToString(sum[:])
	mac := hmac.New(sha256.New, []byte(bootstrapID))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

// tunneledProofSrv builds a relay-wired server, mints the LAN PIN+bootstrap_id,
// and returns both the server and the minted bootstrap_id so a test can compute
// the expected proof for the issued ceremony_token.
func tunneledProofSrv(t *testing.T) (*Server, string) {
	t.Helper()
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "tunnel-marker-secret"
	srv := New(d)
	_, bid, _, err := srv.ownerAccess().mintEnrollPin()
	if err != nil {
		t.Fatalf("mint pin: %v", err)
	}
	return srv, bid
}

// A TUNNEL-FORWARDED enroll/finish with a CORRECT ceremony-bound bootstrap_proof
// (and a zero-device window) succeeds — closing the relay-sees-PIN HIGH: a relay
// holding only sha256(bootstrap_id) can't compute this proof, so only the genuine
// browser (which holds the raw bootstrap_id from the #b= fragment) can finish.
func TestEnrollFinishTunneledValidProof(t *testing.T) {
	srv, bid := tunneledProofSrv(t)
	pin := srv.ownerAccess().enrollPin
	headers := map[string]string{"X-FTW-Tunnel": "tunnel-marker-secret"}
	rec := softwareEnrollFinishProof(t, srv, "localhost", "http://localhost", headers, "pin="+pin,
		func(tok string, body []byte) string { return "bootstrap_proof=" + jsHMACProof(bid, tok, body) })
	if rec.Code != 200 {
		t.Fatalf("tunneled enroll/finish with valid proof: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if hasOwnerCookie(rec) {
		t.Fatalf("tunneled enroll/finish must not set the %s cookie", ownerAccessCookieName)
	}
}

// A TUNNEL-FORWARDED enroll/finish whose body's top-level device_pubkey was SWAPPED
// after the proof was computed is rejected 403 — closes the device_pubkey-swap HIGH.
// A compromised relay (MITM of the forward) can pass through the user's valid
// WebAuthn attestation and a valid (ceremony-bound) proof while replacing the C4
// device_pubkey with its OWN key. Binding the proof to a hash of the EXACT finish
// body bytes makes that tamper break the proof: the Pi recomputes the HMAC over the
// body it actually received and the compare fails, so the relay-controlled key never
// becomes a trusted device. The honest browser computes the proof over the SAME body
// it POSTs, so it still passes (TestEnrollFinishTunneledValidProof).
func TestEnrollFinishTunneledTamperedDevicePubkey(t *testing.T) {
	srv, bid := tunneledProofSrv(t)
	pin := srv.ownerAccess().enrollPin
	headers := map[string]string{"X-FTW-Tunnel": "tunnel-marker-secret"}
	// The honest browser proof binds the honest body bytes...
	proofFor := func(tok string, body []byte) string {
		return "bootstrap_proof=" + jsHMACProof(bid, tok, body)
	}
	// ...but a MITM relay injects its OWN device_pubkey into the body it forwards,
	// AFTER the proof was computed. The attacker key is a canonical (on-curve) P-256
	// key so it would otherwise be stored verbatim by canonicalDevicePubkey.
	attackerKey := freshCanonicalDeviceKeyHex(t)
	tamper := func(body []byte) []byte {
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("tamper: unmarshal honest body: %v", err)
		}
		m["device_pubkey"] = attackerKey
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("tamper: marshal swapped body: %v", err)
		}
		return out
	}
	rec := softwareEnrollFinishTamper(t, srv, "localhost", "http://localhost", headers, "pin="+pin, proofFor, tamper)
	if rec.Code != 403 {
		t.Fatalf("tunneled enroll/finish with a device_pubkey swapped after the proof must be 403; status=%d body=%q", rec.Code, rec.Body.String())
	}
	// No device may have been persisted with the relay's key.
	devs, err := srv.deps.State.LoadTrustedDevices()
	if err != nil {
		t.Fatalf("load trusted devices: %v", err)
	}
	if len(devs) != 0 {
		t.Fatalf("a tampered finish must persist NO device; got %d", len(devs))
	}
}

// freshCanonicalDeviceKeyHex returns a fresh, canonical-form (lowercase, on-curve)
// 128-hex uncompressed P-256 public key — the exact shape canonicalDevicePubkey
// accepts and stores. Used to model the key a MITM relay would substitute.
func freshCanonicalDeviceKeyHex(t *testing.T) string {
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

// A TUNNEL-FORWARDED enroll/finish with NO bootstrap_proof is rejected 403 — the
// proof is mandatory on the tunneled path.
func TestEnrollFinishTunneledMissingProof(t *testing.T) {
	srv, _ := tunneledProofSrv(t)
	pin := srv.ownerAccess().enrollPin
	headers := map[string]string{"X-FTW-Tunnel": "tunnel-marker-secret"}
	rec := softwareEnrollFinishProof(t, srv, "localhost", "http://localhost", headers, "pin="+pin, nil)
	if rec.Code != 403 {
		t.Fatalf("tunneled enroll/finish without proof must be 403; status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// A TUNNEL-FORWARDED enroll/finish with a WRONG bootstrap_proof is rejected 403.
func TestEnrollFinishTunneledWrongProof(t *testing.T) {
	srv, _ := tunneledProofSrv(t)
	pin := srv.ownerAccess().enrollPin
	headers := map[string]string{"X-FTW-Tunnel": "tunnel-marker-secret"}
	rec := softwareEnrollFinishProof(t, srv, "localhost", "http://localhost", headers, "pin="+pin,
		func(tok string, body []byte) string {
			// HMAC computed with the WRONG key (a relay that only knows the
			// sha256 digest, not the raw bootstrap_id, produces something like this).
			return "bootstrap_proof=" + jsHMACProof("not-the-real-bootstrap-id", tok, body)
		})
	if rec.Code != 403 {
		t.Fatalf("tunneled enroll/finish with wrong proof must be 403; status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// A TUNNEL-FORWARDED enroll/finish whose zero-device window CLOSED between
// start and finish is rejected 403 — the source-of-truth backstop for the
// BLOCKER (two concurrent ceremonies both pass enroll/start in the zero-device
// window, the first finishes + saves, the second's finish must NOT also enroll).
// Even a correct proof can't reopen the window. We model the race by seeding a
// device AFTER enroll/start (inside the finishQuery callback) but BEFORE finish.
func TestEnrollFinishTunneledWindowClosed(t *testing.T) {
	srv, bid := tunneledProofSrv(t)
	pin := srv.ownerAccess().enrollPin
	headers := map[string]string{"X-FTW-Tunnel": "tunnel-marker-secret"}
	wallet, err := srv.ownerWalletHandle()
	if err != nil {
		t.Fatalf("wallet handle: %v", err)
	}
	rec := softwareEnrollFinishProof(t, srv, "localhost", "http://localhost", headers, "pin="+pin,
		func(tok string, body []byte) string {
			// A concurrent ceremony just won the race and saved the first device.
			if err := srv.deps.State.SaveTrustedDevice(state.TrustedDevice{
				CredentialID: []byte("already-enrolled"), PublicKey: []byte("k"),
				FriendlyName: "seed", WalletHandle: string(wallet),
				CreatedAtMs: time.Now().UnixMilli(),
			}); err != nil {
				t.Fatalf("seed device: %v", err)
			}
			return "bootstrap_proof=" + jsHMACProof(bid, tok, body)
		})
	if rec.Code != 403 {
		t.Fatalf("tunneled enroll/finish after the zero-device window closed must be 403; status=%d body=%q", rec.Code, rec.Body.String())
	}
}

// An UNTUNNELED LAN enroll/finish needs NO bootstrap_proof and NO zero-device
// recheck — it still succeeds and still issues the ftw_owner cookie (R4 split
// preserved). The proof gate is tunneled-only.
func TestEnrollFinishLANNoProofIssuesCookie(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	// Relay IS wired, but this finish is unmarked (genuine LAN), so it must not
	// require the proof. rpID/origin match minDeps' WebAuthn config.
	d.TunnelMarker = "tunnel-marker-secret"
	srv := New(d)
	rec := softwareEnrollFinishProof(t, srv, "localhost", "http://localhost", nil, "", nil)
	if rec.Code != 200 {
		t.Fatalf("untunneled LAN enroll/finish: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if !hasOwnerCookie(rec) {
		t.Fatalf("untunneled LAN enroll/finish must set the %s session cookie; got %+v",
			ownerAccessCookieName, rec.Result().Cookies())
	}
}
