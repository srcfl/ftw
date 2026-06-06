package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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
	authData = append(authData, 0, 0, 0, 0) // signCount = 0
	authData = append(authData, make([]byte, 16)...) // AAGUID = all-zero
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

	path := "/api/owner-access/enroll/finish?ceremony_token=" + start.CeremonyToken
	if authQuery != "" {
		path += "&" + authQuery
	}
	finishReq := httptest.NewRequest("POST", path, strings.NewReader(string(finishJSON)))
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

// A TUNNEL-FORWARDED enroll/finish (X-FTW-Tunnel marker set, valid PIN) must
// succeed (the directory seed completes) but issue NO ftw_owner cookie — the
// owner session must never traverse the relay tunnel.
func TestEnrollFinishTunneledSuppressesCookie(t *testing.T) {
	d := minDeps(t)
	d.OwnerAccessLANBypass = true
	d.TunnelMarker = "tunnel-marker-secret"
	srv := New(d)

	// Mint a PIN (LAN) so the tunneled enroll passes enrollAllowed's PIN gate.
	pin, _, _, err := srv.ownerAccess().mintEnrollPin()
	if err != nil {
		t.Fatalf("mint pin: %v", err)
	}

	headers := map[string]string{"X-FTW-Tunnel": "tunnel-marker-secret"}
	rec := softwareEnrollFinish(t, srv, "localhost", "http://localhost", headers, "pin="+pin)
	if rec.Code != 200 {
		t.Fatalf("tunneled enroll/finish: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if hasOwnerCookie(rec) {
		t.Fatalf("tunneled enroll/finish must NOT set the %s session cookie (it must never traverse the relay); got cookies %+v",
			ownerAccessCookieName, rec.Result().Cookies())
	}
}
