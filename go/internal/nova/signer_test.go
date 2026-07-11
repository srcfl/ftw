package nova

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"path/filepath"
	"testing"
)

// TestSignerIsTheIdentityKey proves Signer() exposes the SAME ES256 key that
// SignRawHex uses — so a DTLS certificate minted from Signer() and a fingerprint
// signature minted by SignRawHex are bound to one identity.
func TestSignerIsTheIdentityKey(t *testing.T) {
	id, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	signer := id.Signer()
	if signer == nil {
		t.Fatal("Signer() returned nil")
	}
	pub, ok := signer.Public().(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		t.Fatalf("Signer().Public() is not a P-256 key: %T", signer.Public())
	}

	const msg = "ftw-dtls-fp:v1:site:test:abcd:1"
	sigHex, err := id.SignRawHex(msg)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != 64 {
		t.Fatalf("bad sig hex: %v len=%d", err, len(sig))
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(msg))
	if !ecdsa.Verify(pub, h[:], r, s) {
		t.Fatal("SignRawHex signature does not verify under Signer().Public() — Signer is not the identity key")
	}
}
