package nova

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// Identity is the ES256 keypair this forty-two-watts instance uses to
// authenticate with Nova. The private key signs MQTT auth JWTs (see
// SignJWT) and claim-flow proof messages (see SignClaimMessage).
type Identity struct {
	priv *ecdsa.PrivateKey
}

// LoadOrCreateIdentity reads an ES256 private key from path, or generates
// and writes one if the file does not exist. The file format is a
// standard PEM-encoded EC private key (matching Nova's own key layout
// so the same PEM is interoperable).
//
// The enclosing directory is created with 0700 if missing; the key file
// is written with 0600.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	if path == "" {
		return nil, errors.New("nova: key path is empty")
	}
	if _, err := os.Stat(path); err == nil {
		return loadIdentity(path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("nova: mkdir keydir: %w", err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("nova: generate key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("nova: marshal key: %w", err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return nil, fmt.Errorf("nova: write key: %w", err)
	}
	return &Identity{priv: priv}, nil
}

func loadIdentity(path string) (*Identity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("nova: read key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("nova: %s: no PEM block", path)
	}
	priv, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nova: parse key: %w", err)
	}
	if priv.Curve != elliptic.P256() {
		return nil, fmt.Errorf("nova: key is not P-256")
	}
	return &Identity{priv: priv}, nil
}

// PublicKeyHex returns the uncompressed P-256 public key as the 64-byte
// X||Y hex string (128 hex chars) that Nova's auth methods table stores.
// This is what POST /gateways/claim expects in the `public_key` field.
func (id *Identity) PublicKeyHex() string {
	x := padBig(id.priv.X, 32)
	y := padBig(id.priv.Y, 32)
	buf := make([]byte, 0, 64)
	buf = append(buf, x...)
	buf = append(buf, y...)
	return hex.EncodeToString(buf)
}

// Signer returns the identity's private key as a crypto.Signer so callers
// (e.g. pion's webrtc.NewCertificate, which mints the home-route DTLS cert) can
// bind an X.509 certificate to this identity without the key ever leaving the
// process. It is the SAME key that signs Nova JWTs, claim proofs, /me/register,
// and the DTLS fingerprint — see the domain-separation note on SignRawHex.
func (id *Identity) Signer() crypto.Signer { return id.priv }

// SignRawHex signs msg with the identity's ES256 key and returns the raw
// R||S 64-byte signature as a 128-char hex string. This is the exact
// format Nova's ownership.verifyES256Signature expects for claim proofs.
//
// DOMAIN SEPARATION (load-bearing): this key signs SHA-256(msg) with NO built-in
// domain tag, and it is reused across protocols (JWTs sign base64url(header)."."
// base64url(payload); /me/register signs MeRegisterSigningString; the home route
// signs DtlsFingerprintSigningString). Every caller MUST therefore prefix msg
// with a unique, versioned "ftw-<purpose>:v1:" tag so a signature minted for one
// purpose can never be replayed as another. NEVER pass attacker-influenced bytes
// to SignRawHex without such a prefix.
func (id *Identity) SignRawHex(msg string) (string, error) {
	hash := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, id.priv, hash[:])
	if err != nil {
		return "", fmt.Errorf("nova: sign: %w", err)
	}
	sig := make([]byte, 64)
	rb := r.Bytes()
	sb := s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return hex.EncodeToString(sig), nil
}

func padBig(x *big.Int, size int) []byte {
	b := x.Bytes()
	if len(b) >= size {
		return b
	}
	out := make([]byte, size)
	copy(out[size-len(b):], b)
	return out
}
