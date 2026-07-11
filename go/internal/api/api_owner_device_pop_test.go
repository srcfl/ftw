package api

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/frahlg/forty-two-watts/go/internal/state"
)

// newDeviceKey mints a P-256 device key and returns (private key, 128-hex
// uncompressed X||Y public key) in the canonical lowercase form the Pi stores.
func newDeviceKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	x := priv.PublicKey.X.Bytes()
	y := priv.PublicKey.Y.Bytes()
	buf := make([]byte, 64)
	copy(buf[32-len(x):32], x)
	copy(buf[64-len(y):64], y)
	return priv, hex.EncodeToString(buf)
}

// signDevicePoP signs the C3 string for (site, challenge) and returns the raw
// r||s 64-byte signature, base64url (no padding) — exactly what WebCrypto emits.
func signDevicePoP(t *testing.T, priv *ecdsa.PrivateKey, site, challenge string) string {
	t.Helper()
	msg := "ftw-device-pop:v1:" + site + ":" + challenge
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	sig := make([]byte, 64)
	rb, sb := r.Bytes(), s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return base64.RawURLEncoding.EncodeToString(sig)
}

// mintChallenge calls the open device-challenge endpoint and returns its nonce.
func mintChallenge(t *testing.T, srv *Server) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/owner-access/device-challenge", nil)
	req.Host = "1.2.3.4"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("device-challenge status=%d body=%q", rec.Code, rec.Body.String())
	}
	var out struct {
		Challenge string `json:"challenge"`
		ExpMs     int64  `json:"exp_ms"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode challenge: %v", err)
	}
	if out.Challenge == "" || out.ExpMs == 0 {
		t.Fatalf("empty challenge/exp: %q", rec.Body.String())
	}
	return out.Challenge
}

func postDevicePoP(t *testing.T, srv *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/api/owner-access/device-pop", bytes.NewReader(b))
	req.Host = "1.2.3.4"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// devicePoPDeps builds a Server whose gate is ACTIVE (TunnelMarker set) so the
// open-path wiring is genuinely exercised, with a known SiteID for the signing
// string and a pinned trusted device carrying devicePubkey.
func devicePoPDeps(t *testing.T, devicePubkey string) (*Server, *Deps) {
	t.Helper()
	d := minDeps(t)
	d.SiteID = "site:test-site"
	d.TunnelMarker = "marker" // gate active — device-pop must still be reachable (open path)
	if devicePubkey != "" {
		if err := d.State.SaveTrustedDevice(state.TrustedDevice{
			CredentialID: []byte("owner-cred"), PublicKey: []byte("k"),
			FriendlyName: "owner phone", CreatedAtMs: time.Now().UnixMilli(),
			DevicePubkey: devicePubkey,
		}); err != nil {
			t.Fatalf("seed device: %v", err)
		}
	}
	return New(d), d
}

// Valid PoP against a pinned device mints an ftw_owner session.
func TestDevicePoPValidIssuesSession(t *testing.T) {
	priv, pub := newDeviceKey(t)
	srv, d := devicePoPDeps(t, pub)

	ch := mintChallenge(t, srv)
	sig := signDevicePoP(t, priv, d.SiteID, ch)
	rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": ch, "sig": sig,
	})
	if rec.Code != 200 {
		t.Fatalf("valid PoP status=%d body=%q", rec.Code, rec.Body.String())
	}
	// A session cookie must be set, and it must authenticate a subsequent
	// (gated, tunnelled) request.
	cookies := rec.Result().Cookies()
	var sess *http.Cookie
	for _, c := range cookies {
		if c.Name == ownerAccessCookieName && c.Value != "" {
			sess = c
		}
	}
	if sess == nil {
		t.Fatalf("no %s session cookie issued: %+v", ownerAccessCookieName, cookies)
	}
	// Tunnelled request (remote) carrying the minted cookie is authorized.
	req := httptest.NewRequest("GET", "/api/owner-access/whoami", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("X-FTW-Tunnel", "marker")
	req.AddCookie(sess)
	authRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(authRec, req)
	if authRec.Code != 200 {
		t.Fatalf("minted session should authenticate a tunnelled request: %d %q", authRec.Code, authRec.Body.String())
	}
}

// A device key that is NOT pinned is rejected even with a valid signature.
func TestDevicePoPUnknownPubkeyRejected(t *testing.T) {
	// Pin one key, present a different (valid, on-curve) one.
	_, pinned := newDeviceKey(t)
	srv, d := devicePoPDeps(t, pinned)

	strangerPriv, strangerPub := newDeviceKey(t)
	ch := mintChallenge(t, srv)
	sig := signDevicePoP(t, strangerPriv, d.SiteID, ch)
	rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": strangerPub, "challenge": ch, "sig": sig,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown pubkey must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("no session must be minted for an unknown device key")
	}
}

// A pinned device key with a signature that does NOT verify (wrong signer) is
// rejected. The pubkey is in the set, but the sig was made by another key.
func TestDevicePoPBadSigRejected(t *testing.T) {
	_, pub := newDeviceKey(t)
	srv, d := devicePoPDeps(t, pub)

	// Sign with a DIFFERENT private key than the pinned pubkey.
	wrongPriv, _ := newDeviceKey(t)
	ch := mintChallenge(t, srv)
	badSig := signDevicePoP(t, wrongPriv, d.SiteID, ch)
	rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": ch, "sig": badSig,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bad signature must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// A signature over a DIFFERENT site binding must not verify against this Pi's
// SiteID (domain + site separation in the signing string).
func TestDevicePoPWrongSiteBindingRejected(t *testing.T) {
	priv, pub := newDeviceKey(t)
	srv, _ := devicePoPDeps(t, pub)

	ch := mintChallenge(t, srv)
	// Sign with the correct key + challenge but a foreign site.
	sig := signDevicePoP(t, priv, "site:somewhere-else", ch)
	rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": ch, "sig": sig,
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-site signature must be 403, got %d body=%q", rec.Code, rec.Body.String())
	}
}

// An expired (or unknown) challenge is rejected even with an otherwise-valid
// signature, and a consumed challenge cannot be replayed.
func TestDevicePoPExpiredAndReplayRejected(t *testing.T) {
	priv, pub := newDeviceKey(t)
	srv, d := devicePoPDeps(t, pub)

	// 1) Unknown challenge (never minted) → 403.
	bogus := "this-challenge-was-never-minted"
	sig := signDevicePoP(t, priv, d.SiteID, bogus)
	if rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": bogus, "sig": sig,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown challenge must be 403, got %d", rec.Code)
	}

	// 2) Expired challenge: mint one, then force it stale in the map.
	ch := mintChallenge(t, srv)
	oa := srv.ownerAccess()
	oa.mu.Lock()
	oa.devicePoPChallenges[ch] = time.Now().Add(-time.Second)
	oa.mu.Unlock()
	expSig := signDevicePoP(t, priv, d.SiteID, ch)
	if rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": ch, "sig": expSig,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("expired challenge must be 403, got %d", rec.Code)
	}

	// 3) Replay: a fresh challenge consumed by one valid PoP cannot be reused.
	ch2 := mintChallenge(t, srv)
	sig2 := signDevicePoP(t, priv, d.SiteID, ch2)
	if rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": ch2, "sig": sig2,
	}); rec.Code != 200 {
		t.Fatalf("first PoP should succeed, got %d body=%q", rec.Code, rec.Body.String())
	}
	if rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": ch2, "sig": sig2,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("replayed challenge must be 403, got %d", rec.Code)
	}
}

// A wrong-signature attempt CONSUMES the challenge (single-use), so a subsequent
// correct signature against the same challenge is rejected — no oracle to retry.
func TestDevicePoPWrongSigConsumesChallenge(t *testing.T) {
	priv, pub := newDeviceKey(t)
	srv, d := devicePoPDeps(t, pub)

	ch := mintChallenge(t, srv)
	wrongPriv, _ := newDeviceKey(t)
	badSig := signDevicePoP(t, wrongPriv, d.SiteID, ch)
	if rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": ch, "sig": badSig,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("bad sig must be 403, got %d", rec.Code)
	}
	// Now the correct sig over the SAME challenge must fail — it was consumed.
	goodSig := signDevicePoP(t, priv, d.SiteID, ch)
	if rec := postDevicePoP(t, srv, map[string]any{
		"device_pubkey": pub, "challenge": ch, "sig": goodSig,
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("challenge must be consumed by the failed attempt, got %d", rec.Code)
	}
}

// Malformed inputs (missing fields, non-canonical pubkey) are rejected without
// touching the DB.
func TestDevicePoPMalformedInputs(t *testing.T) {
	priv, pub := newDeviceKey(t)
	srv, d := devicePoPDeps(t, pub)
	ch := mintChallenge(t, srv)
	sig := signDevicePoP(t, priv, d.SiteID, ch)

	// Missing sig → 400.
	if rec := postDevicePoP(t, srv, map[string]any{"device_pubkey": pub, "challenge": ch}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing sig must be 400, got %d", rec.Code)
	}
	// Uppercase (non-canonical) pubkey → 403 (rejected before DB).
	upper := pub[:64] + "AA" + pub[66:]
	if rec := postDevicePoP(t, srv, map[string]any{"device_pubkey": upper, "challenge": ch, "sig": sig}); rec.Code != http.StatusForbidden {
		t.Fatalf("non-canonical pubkey must be 403, got %d", rec.Code)
	}
}

// validDevicePubKeyHex is the canonical-form gate; verify it matches the relay's
// contract: 128 lowercase hex, on-curve.
func TestValidDevicePubKeyHex(t *testing.T) {
	_, pub := newDeviceKey(t)
	if !validDevicePubKeyHex(pub) {
		t.Fatalf("freshly-minted key should be valid: %s", pub)
	}
	cases := []string{
		"",                  // empty
		pub[:126],           // too short
		pub + "00",          // too long
		pub[:64] + "GG" + pub[66:], // non-hex
		// All-zero X||Y is not on the curve.
		"00000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
	}
	for i, c := range cases {
		if validDevicePubKeyHex(c) {
			t.Errorf("case %d should be invalid: %q", i, c)
		}
	}
	// An uppercase-but-otherwise-valid key is rejected (canonical lowercase only).
	upper := pub[:2] + "AB" + pub[4:]
	if validDevicePubKeyHex(upper) {
		t.Errorf("uppercase key must be rejected (canonical lowercase only): %q", upper)
	}
}

// C4: device_pubkey supplied on enroll/finish must persist on the new
// credential's row. We can't run a full WebAuthn ceremony in a unit test, so we
// exercise the extraction helper + the state write the handler performs.
func TestEnrollDevicePubkeyExtractionAndPersist(t *testing.T) {
	_, pub := newDeviceKey(t)
	// The finish body is the WebAuthn attestation JSON plus our extra field.
	body := []byte(`{"id":"abc","rawId":"abc","type":"public-key","response":{"clientDataJSON":"x","attestationObject":"y"},"device_pubkey":"` + pub + `"}`)
	got := extractDevicePubkeyField(body)
	if got != pub {
		t.Fatalf("extracted device_pubkey = %q, want %q", got, pub)
	}
	if canonicalDevicePubkey(got) != pub {
		t.Fatalf("canonical form mismatch: %q", canonicalDevicePubkey(got))
	}
	// A missing field extracts to "".
	if extractDevicePubkeyField([]byte(`{"id":"abc"}`)) != "" {
		t.Fatalf("absent field must extract to empty")
	}
	// A non-canonical value canonicalises to "" (dropped, passkey still enrolls).
	if canonicalDevicePubkey("not-a-key") != "" {
		t.Fatalf("invalid key must canonicalise to empty")
	}

	// And the state write the handler performs round-trips + publishes.
	d := minDeps(t)
	if err := d.State.SaveTrustedDevice(state.TrustedDevice{
		CredentialID: []byte("new-cred"), PublicKey: []byte("k"),
		FriendlyName: "phone", CreatedAtMs: time.Now().UnixMilli(),
		DevicePubkey: canonicalDevicePubkey(got),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	pks, err := d.State.TrustedDevicePubkeys()
	if err != nil {
		t.Fatalf("pubkeys: %v", err)
	}
	if len(pks) != 1 || pks[0] != pub {
		t.Fatalf("enrolled device_pubkey not published: %v", pks)
	}
}

// The device-pop open path is reachable even when the gate is active and the
// request is tunnelled (remote) — it is the silent-login surface, so it must NOT
// require a pre-existing session. (It still mints nothing without a valid PoP.)
func TestDevicePoPOpenPathWhenGated(t *testing.T) {
	_, pub := newDeviceKey(t)
	srv, _ := devicePoPDeps(t, pub)
	// Tunnelled GET of device-challenge with NO session must reach the handler
	// (200), not be 401'd by the gate.
	req := httptest.NewRequest("GET", "/api/owner-access/device-challenge", nil)
	req.Host = "127.0.0.1"
	req.Header.Set("X-FTW-Tunnel", "marker")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("device-challenge must be an open path under the active gate, got %d body=%q", rec.Code, rec.Body.String())
	}
}
