package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
)

// verifyES256Hex verifies a raw R||S ECDSA-P256 signature (128 hex chars) of
// msg against an uncompressed X||Y public key (128 hex chars). This is the
// exact wire format produced by nova.Identity.SignRawHex / PublicKeyHex, which
// the owner Pi uses to sign POST /me/register. Returns false on any decode,
// length, on-curve, or verification failure — never panics on attacker input.
func verifyES256Hex(pubKeyHex, msg, sigHex string) bool {
	pub, err := parseP256PubKeyHex(pubKeyHex)
	if err != nil {
		return false
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != 64 {
		return false
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(msg))
	return ecdsa.Verify(pub, h[:], r, s)
}

// verifyES256B64URL verifies a raw R||S ECDSA-P256 signature, base64url-encoded
// (no padding), of msg against an uncompressed X||Y public key (128 hex chars).
//
// This is the WebCrypto wire format: SubtleCrypto.sign("ECDSA", {hash:"SHA-256"})
// over a P-256 key produces the 64-byte raw r||s the browser then base64url's.
// It is the device-key proof format for the signaling-offer challenge (C2): the
// browser proves possession of a device key the Pi trusts before the relay will
// forward its offer to the Pi. Returns false on any decode, length, on-curve, or
// verification failure — never panics on attacker input.
func verifyES256B64URL(pubKeyHex, msg, sigB64URL string) bool {
	pub, err := parseP256PubKeyHex(pubKeyHex)
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

// validDevicePubKeyHex reports whether s is a syntactically valid uncompressed
// P-256 public key in the device-key wire format: 128 lowercase hex chars that
// decode to a point on the curve. Used to bound + canonicalise the device keys
// the Pi publishes (C1) and the browser presents (C2) so the stored set is
// always comparable byte-for-byte.
func validDevicePubKeyHex(s string) bool {
	if len(s) != 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false // reject uppercase + non-hex so the set is canonical
		}
	}
	_, err := parseP256PubKeyHex(s)
	return err == nil
}

// parseP256PubKeyHex parses a 64-byte (128 hex char) uncompressed P-256 public
// key as X||Y, rejecting anything that is not a valid point on the curve.
func parseP256PubKeyHex(s string) (*ecdsa.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(b) != 64 {
		return nil, errors.New("public key must be 64 bytes (X||Y)")
	}
	x := new(big.Int).SetBytes(b[:32])
	y := new(big.Int).SetBytes(b[32:])
	if !elliptic.P256().IsOnCurve(x, y) {
		return nil, errors.New("public key point is not on P-256")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
}
