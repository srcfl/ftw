package p2p

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/frahlg/forty-two-watts/go/internal/nova"
	"github.com/frahlg/forty-two-watts/go/internal/tunnel"
)

const testFpToken = "AB:CD:01:EF:23:45:67:89:AB:CD:01:EF:23:45:67:89:AB:CD:01:EF:23:45:67:89:AB:CD:01:EF:23:45:67:89"

// TestSignFingerprint proves the Pi signs the DTLS fingerprint of an answer such
// that a browser holding only the pinned public key can verify it — the entire
// anti-relay-MITM mechanism.
func TestSignFingerprint(t *testing.T) {
	id, err := nova.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(slog.Default(), nil)
	m.SetSigner("site:Home", id)

	sdp := "v=0\r\na=group:BUNDLE 0\r\na=fingerprint:sha-256 " + testFpToken + "\r\n"
	sig, ts := m.SignFingerprint(sdp)
	if sig == "" || ts == 0 {
		t.Fatalf("expected a signature, got sig=%q ts=%d", sig, ts)
	}

	// Verify exactly as the browser will: rebuild the canonical signed string
	// from the normalized fingerprint + ts, and check the detached ES256 sig
	// against the (pinned) public key.
	want := tunnel.DtlsFingerprintSigningString("site:Home", tunnel.NormalizeDtlsFingerprint(testFpToken), ts)
	if !verifyES256(t, id.PublicKeyHex(), want, sig) {
		t.Fatal("signature does not verify under the pinned identity key")
	}

	// No signer → unsigned (a verifying browser then rejects the answer).
	bare := NewManager(slog.Default(), nil)
	if s, ts := bare.SignFingerprint(sdp); s != "" || ts != 0 {
		t.Fatalf("no-signer must yield empty, got %q %d", s, ts)
	}
	// No fingerprint in the SDP → nothing to sign.
	if s, ts := m.SignFingerprint("v=0\r\n"); s != "" || ts != 0 {
		t.Fatalf("no-fingerprint must yield empty, got %q %d", s, ts)
	}
}

func verifyES256(t *testing.T, pubHex, msg, sigHex string) bool {
	t.Helper()
	pb, err := hex.DecodeString(pubHex)
	if err != nil || len(pb) != 64 {
		t.Fatalf("bad pubkey hex: %v len=%d", err, len(pb))
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(),
		X: new(big.Int).SetBytes(pb[:32]), Y: new(big.Int).SetBytes(pb[32:])}
	sb, err := hex.DecodeString(sigHex)
	if err != nil || len(sb) != 64 {
		t.Fatalf("bad sig hex: %v len=%d", err, len(sb))
	}
	h := sha256.Sum256([]byte(msg))
	return ecdsa.Verify(pub, h[:], new(big.Int).SetBytes(sb[:32]), new(big.Int).SetBytes(sb[32:]))
}
