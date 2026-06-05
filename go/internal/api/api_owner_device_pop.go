// api_owner_device_pop.go
//
// C3 — silent device-key proof-of-possession login over the P2P channel.
//
// At LAN enrollment (C4) the browser mints a non-extractable P-256 "device key"
// and the Pi pins its public half on the new credential's trusted_devices row.
// On a later visit the browser can then mint an owner session SILENTLY — no
// passkey prompt — by proving possession of that pinned device key over the open
// (already-authenticated-by-the-P2P-channel) home route:
//
//	GET  /api/owner-access/device-challenge -> {"challenge":"<b64url>","exp_ms":<int64>}
//	POST /api/owner-access/device-pop        body {"device_pubkey","challenge","sig"}
//	     sig = ECDSA-P256(SHA-256( "ftw-device-pop:v1:<site>:<challenge>" )), raw r||s, base64url
//
// On a valid PoP the Pi issues the SAME ftw_owner session a passkey login mints
// (issueOwnerSession). Both paths are OPEN (pre-session) — see
// isOwnerAccessOpenPath — because the device key IS the credential here, exactly
// like a passkey assertion is on login/finish. The challenge is single-use, has a
// short TTL, and is consumed on the POST so a captured PoP can't be replayed.
//
// Step-up / sensitive owner-management actions still require a fresh passkey
// (authorizeOwnerManage gates those independently); a silent device session is a
// convenience login, not an escalation.
package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"
)

// ownerDevicePoPChallengeTTL bounds the lifetime of a device-PoP challenge.
// ~60s is plenty for the browser to sign and POST back; short enough that a
// leaked challenge is useless almost immediately.
const ownerDevicePoPChallengeTTL = 60 * time.Second

// maxDevicePoPChallenges caps the in-flight challenge map. device-challenge is an
// open (pre-session) path, so without a cap a P2P client could grow Pi memory
// within the TTL window. A real browser holds at most one open challenge.
const maxDevicePoPChallenges = 64

// maxDevicePoPBody bounds the POST body the device-pop handler decodes. A real
// body is a few hundred bytes (a 128-hex key, a b64url challenge, a b64url sig).
const maxDevicePoPBody = 8 << 10

// devicePoPSigningString is the canonical message the browser signs for the C3
// proof-of-possession. Domain-separated ("ftw-device-pop:v1:") and bound to BOTH
// the site and the single-use challenge so a signature minted here can never be
// replayed against another site or another path (e.g. the C2 signaling offer,
// which uses the distinct "ftw-signal:v1:" tag). Both ends MUST build this
// identically — that is the entire point of pinning the format in one place.
func devicePoPSigningString(site, challenge string) string {
	return fmt.Sprintf("ftw-device-pop:v1:%s:%s", site, challenge)
}

// verifyDevicePoPSig verifies a raw R||S ECDSA-P256 signature, base64url-encoded
// (no padding), of msg against an uncompressed X||Y device public key (128 hex
// chars). This is the WebCrypto wire format: SubtleCrypto.sign("ECDSA",
// {hash:"SHA-256"}) over a P-256 key yields the 64-byte raw r||s the browser then
// base64url's. It mirrors the relay's verifyES256B64URL byte-for-byte (C2/C3 share
// the format). Returns false on any decode, length, on-curve, or verification
// failure — never panics on attacker input.
func verifyDevicePoPSig(pubKeyHex, msg, sigB64URL string) bool {
	pub, err := parseDevicePubKeyHex(pubKeyHex)
	if err != nil {
		return false
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

// parseDevicePubKeyHex parses a 64-byte (128 hex char) uncompressed P-256 public
// key as X||Y, rejecting anything that is not a valid point on the curve. Mirrors
// the relay's parseP256PubKeyHex so the Pi and relay agree byte-for-byte on what
// a device key is.
func parseDevicePubKeyHex(s string) (*ecdsa.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 64 {
		return nil, errors.New("device pubkey must be 64 bytes (X||Y)")
	}
	x := new(big.Int).SetBytes(b[:32])
	y := new(big.Int).SetBytes(b[32:])
	if !elliptic.P256().IsOnCurve(x, y) {
		return nil, errors.New("device pubkey point is not on P-256")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
}

// validDevicePubKeyHex reports whether s is a syntactically valid uncompressed
// P-256 public key in the canonical device-key wire format: 128 LOWERCASE hex
// chars that decode to a point on the curve. Rejecting uppercase keeps the stored
// set byte-for-byte comparable with what the relay publishes (C1). Mirrors the
// relay's validDevicePubKeyHex.
func validDevicePubKeyHex(s string) bool {
	if len(s) != 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	_, err := parseDevicePubKeyHex(s)
	return err == nil
}

// extractDevicePubkeyField pulls the optional top-level "device_pubkey" string
// out of a WebAuthn finish body without disturbing the rest of it. Returns "" on
// any decode failure or when the field is absent — the caller treats that as "no
// device key supplied" and enrolls the passkey anyway. Tolerant by design: the
// device key is an additive convenience on top of the passkey.
func extractDevicePubkeyField(body []byte) string {
	var probe struct {
		DevicePubkey string `json:"device_pubkey"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.DevicePubkey
}

// canonicalDevicePubkey returns s iff it is a syntactically valid canonical
// (lowercase, on-curve) 128-hex P-256 device key, else "". Storing only canonical
// keys keeps the published set (C1) byte-for-byte comparable with what the relay
// and browser present.
func canonicalDevicePubkey(s string) string {
	if validDevicePubKeyHex(s) {
		return s
	}
	return ""
}

// handleOwnerDeviceChallenge mints a fresh single-use device-PoP challenge.
// Open path (pre-session): it leaks nothing — a random nonce the browser must
// sign with a key the Pi already trusts. Bounded + GC'd so a P2P flood can't grow
// memory.
func (s *Server) handleOwnerDeviceChallenge(w http.ResponseWriter, r *http.Request) {
	ch, err := randomToken()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	exp := time.Now().Add(ownerDevicePoPChallengeTTL)
	oa := s.ownerAccess()
	oa.mu.Lock()
	oa.gcDevicePoP()
	capDevicePoP(oa.devicePoPChallenges)
	if oa.devicePoPChallenges == nil {
		oa.devicePoPChallenges = make(map[string]time.Time)
	}
	oa.devicePoPChallenges[ch] = exp
	oa.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"challenge": ch,
		"exp_ms":    exp.UnixMilli(),
	})
}

// handleOwnerDevicePoP verifies a device-key proof-of-possession and, on success,
// mints the same ftw_owner session a passkey login would. Fail-closed at every
// step; the challenge is consumed (single-use) the moment it is recognised so a
// captured PoP can never be replayed.
func (s *Server) handleOwnerDevicePoP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxDevicePoPBody)
	var body struct {
		DevicePubkey string `json:"device_pubkey"`
		Challenge    string `json:"challenge"`
		Sig          string `json:"sig"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if body.DevicePubkey == "" || body.Challenge == "" || body.Sig == "" {
		http.Error(w, "device_pubkey, challenge and sig are required", http.StatusBadRequest)
		return
	}
	// Canonical-form gate first: reject anything that isn't a lowercase 128-hex
	// on-curve key before touching the DB, so the stored-set comparison is always
	// byte-for-byte.
	if !validDevicePubKeyHex(body.DevicePubkey) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Consume the challenge: it must be known AND unexpired AND single-use. We
	// delete it under the lock regardless of the rest of the outcome so a wrong
	// signature can't be retried against the same challenge.
	oa := s.ownerAccess()
	oa.mu.Lock()
	oa.gcDevicePoP()
	exp, known := oa.devicePoPChallenges[body.Challenge]
	if known {
		delete(oa.devicePoPChallenges, body.Challenge)
	}
	oa.mu.Unlock()
	if !known || time.Now().After(exp) {
		http.Error(w, "unknown or expired challenge", http.StatusForbidden)
		return
	}

	// The pubkey must be a PINNED trusted device. Look it up by its device key.
	if s.deps.State == nil {
		http.Error(w, "state store not configured", http.StatusInternalServerError)
		return
	}
	dev, err := s.deps.State.LookupTrustedDeviceByPubkey(body.DevicePubkey)
	if err != nil {
		// Unknown device key (or DB error) — never reveal which. Fail closed.
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Verify the signature over the domain-separated, site- and challenge-bound
	// message. Constant-time pubkey equality isn't needed (the DB lookup already
	// matched), but we re-derive the signing string from the SERVER's site so a
	// client can't smuggle a different binding.
	msg := devicePoPSigningString(s.deps.SiteID, body.Challenge)
	if !verifyDevicePoPSig(body.DevicePubkey, msg, body.Sig) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Bind the minted session to the SAME credential_id the device key pins, so a
	// later device-delete revokes this session exactly like a passkey one. (See
	// handleOwnerDeviceDelete: it revokes by credential_id.)
	if err := s.issueOwnerSession(w, dev.CredentialID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"authenticated": true,
		"friendly_name": dev.FriendlyName,
	})
}

// gcDevicePoP drops expired device-PoP challenges. Caller holds oa.mu.
func (oa *ownerAccessState) gcDevicePoP() {
	now := time.Now()
	for k, exp := range oa.devicePoPChallenges {
		if exp.Before(now) {
			delete(oa.devicePoPChallenges, k)
		}
	}
}

// capDevicePoP evicts the soonest-to-expire entries until the map is below the
// cap, bounding it against an unauthenticated flood within the TTL window. Caller
// holds oa.mu.
func capDevicePoP(m map[string]time.Time) {
	for len(m) >= maxDevicePoPChallenges {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, v := range m {
			if first || v.Before(oldest) {
				oldestKey, oldest, first = k, v, false
			}
		}
		delete(m, oldestKey)
	}
}
