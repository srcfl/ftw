package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
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
