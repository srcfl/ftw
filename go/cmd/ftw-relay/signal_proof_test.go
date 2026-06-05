package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// signal_proof_test.go — shared test helpers for the C2 device-key proof on the
// signaling offer. A device key is an ECDSA P-256 keypair; its public form is the
// uncompressed X||Y as 128 lowercase hex (the device_pubkey wire format), and a
// proof signs "ftw-signal:v1:<site>:<nonce>" as raw r||s, base64url (the same
// WebCrypto format the browser produces).

// deviceKey is a test P-256 device keypair plus its wire-format public key.
type deviceKey struct {
	priv      *ecdsa.PrivateKey
	pubKeyHex string // 128 lowercase hex, uncompressed X||Y
}

// genP256 mints a P-256 keypair. Split out so non-*testing.T callers (a
// package-level var initializer) can reuse it.
func genP256() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// newDeviceKey mints a throwaway P-256 device keypair for tests.
func newDeviceKey(t *testing.T) *deviceKey {
	t.Helper()
	priv, err := genP256()
	if err != nil {
		t.Fatalf("gen device key: %v", err)
	}
	return &deviceKey{priv: priv, pubKeyHex: devicePubKeyHex(priv)}
}

// devicePubKeyHex encodes a P-256 public key as the 128-lowercase-hex X||Y wire
// format (32-byte big-endian X, then Y, left-padded), matching the relay's
// parseP256PubKeyHex.
func devicePubKeyHex(priv *ecdsa.PrivateKey) string {
	x := priv.PublicKey.X.Bytes()
	y := priv.PublicKey.Y.Bytes()
	buf := make([]byte, 64)
	copy(buf[32-len(x):32], x)
	copy(buf[64-len(y):64], y)
	return hex.EncodeToString(buf)
}

// signProof signs the C2 signing string for (siteID, nonce) and returns the raw
// r||s signature as base64url (no padding) — exactly the WebCrypto wire format.
func (d *deviceKey) signProof(t *testing.T, siteID, nonce string) string {
	t.Helper()
	msg := signalProofSigningString(siteID, nonce)
	h := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, d.priv, h[:])
	if err != nil {
		t.Fatalf("sign proof: %v", err)
	}
	sig := make([]byte, 64)
	rb, sb := r.Bytes(), s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return base64.RawURLEncoding.EncodeToString(sig)
}

// offerEnvelope builds the JSON offer body the browser posts under C2: the raw
// SDP plus the device-key proof. Field names are fixed by the wire contract.
func offerEnvelope(t *testing.T, d *deviceKey, siteID, challengeNonce, sdp string) []byte {
	t.Helper()
	b, err := json.Marshal(struct {
		SDP          string `json:"sdp"`
		DevicePubkey string `json:"device_pubkey"`
		Nonce        string `json:"nonce"`
		Sig          string `json:"sig"`
	}{
		SDP:          sdp,
		DevicePubkey: d.pubKeyHex,
		Nonce:        challengeNonce,
		Sig:          d.signProof(t, siteID, challengeNonce),
	})
	if err != nil {
		t.Fatalf("marshal offer envelope: %v", err)
	}
	return b
}

// TestVerifyES256B64URL_RoundTrip proves the relay's base64url verify accepts a
// genuine WebCrypto-style raw r||s signature and rejects a tampered one.
func TestVerifyES256B64URL_RoundTrip(t *testing.T) {
	d := newDeviceKey(t)
	const site = "site:Home"
	const nonce = "Zm9vYmFyZm9vYmFy" // any opaque string; the signer covers it
	sig := d.signProof(t, site, nonce)
	msg := signalProofSigningString(site, nonce)
	if !verifyES256B64URL(d.pubKeyHex, msg, sig) {
		t.Fatal("valid signature must verify")
	}
	// Wrong message → reject.
	if verifyES256B64URL(d.pubKeyHex, signalProofSigningString(site, "other"), sig) {
		t.Fatal("signature over a different nonce must NOT verify")
	}
	// Wrong key → reject.
	other := newDeviceKey(t)
	if verifyES256B64URL(other.pubKeyHex, msg, sig) {
		t.Fatal("signature must NOT verify under a different key")
	}
	// Garbage signature → reject (no panic).
	if verifyES256B64URL(d.pubKeyHex, msg, "!!!notb64!!!") {
		t.Fatal("malformed signature must NOT verify")
	}
}

// TestValidDevicePubKeyHex bounds the device-key wire format: 128 lowercase hex
// on the curve, nothing else.
func TestValidDevicePubKeyHex(t *testing.T) {
	d := newDeviceKey(t)
	if !validDevicePubKeyHex(d.pubKeyHex) {
		t.Fatal("a real device pubkey must be valid")
	}
	bad := []string{
		"",
		"abcd",                               // too short
		d.pubKeyHex + "00",                   // too long
		"g" + d.pubKeyHex[1:],                // non-hex
		"ABCDEF" + d.pubKeyHex[6:],           // uppercase rejected
		hex.EncodeToString(make([]byte, 64)), // all-zero: not on curve
	}
	for _, s := range bad {
		if validDevicePubKeyHex(s) {
			t.Errorf("invalid device pubkey accepted: %q", s)
		}
	}
}
