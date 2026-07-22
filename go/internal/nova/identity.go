package nova

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
)

// Identity is the ES256 keypair this FTW instance uses to
// authenticate with Nova. The private key signs MQTT auth JWTs (see
// SignJWT) and claim-flow proof messages (see SignClaimMessage).
type Identity struct {
	priv *ecdsa.PrivateKey
}

// LoadOrCreateIdentity reads an ES256 private key from path, or generates
// and writes one atomically if the file does not exist. The file format is a
// standard PEM-encoded EC private key (matching Nova's own key layout
// so the same PEM is interoperable).
//
// The enclosing directory is created with 0700 if missing; the key file
// is written with 0600.
func LoadOrCreateIdentity(path string) (*Identity, error) {
	return loadOrCreateIdentity(path, syncDirectory)
}

func loadOrCreateIdentity(path string, syncDir func(string) error) (*Identity, error) {
	if path == "" {
		return nil, errors.New("nova: key path is empty")
	}
	if _, err := os.Stat(path); err == nil {
		return loadIdentity(path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("nova: stat key: %w", err)
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
	installed, err := writeFileNoReplace(path, out, 0o600, syncDir)
	if err != nil {
		return nil, fmt.Errorf("nova: write key: %w", err)
	}
	if !installed {
		return loadIdentity(path)
	}
	return &Identity{priv: priv}, nil
}

// writeFileNoReplace installs a fully synced temp file only when path does not
// exist. A concurrent creator wins without having its key replaced.
func writeFileNoReplace(path string, data []byte, perm fs.FileMode, syncDir func(string) error) (bool, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		return false, err
	}
	if err := os.Remove(tmpPath); err != nil {
		return true, err
	}
	if err := syncDir(dir); err != nil {
		return true, err
	}
	return true, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
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
	return hex.EncodeToString(id.PublicKeyBytes())
}

// PublicKeyBytes returns the raw 64-byte X||Y public key.
func (id *Identity) PublicKeyBytes() []byte {
	x := padBig(id.priv.X, 32)
	y := padBig(id.priv.Y, 32)
	buf := make([]byte, 0, 64)
	buf = append(buf, x...)
	return append(buf, y...)
}

// SignRawHex signs msg with the identity's ES256 key and returns the raw
// R||S 64-byte signature as a 128-char hex string. This is the exact
// format Nova's ownership.verifyES256Signature expects for claim proofs.
//
// DOMAIN SEPARATION: this key also signs JWTs. Every raw-message caller must
// prefix msg with a unique, versioned purpose tag. Never pass
// attacker-influenced bytes without that prefix.
func (id *Identity) SignRawHex(msg string) (string, error) {
	sig, err := id.SignRaw([]byte(msg))
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig), nil
}

// SignRaw hashes msg with SHA-256 and returns a raw 64-byte R||S signature.
// Callers must start msg with a unique, versioned purpose tag.
func (id *Identity) SignRaw(msg []byte) ([]byte, error) {
	hash := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, id.priv, hash[:])
	if err != nil {
		return nil, fmt.Errorf("nova: sign: %w", err)
	}
	sig := make([]byte, 64)
	rb := r.Bytes()
	sb := s.Bytes()
	copy(sig[32-len(rb):32], rb)
	copy(sig[64-len(sb):64], sb)
	return sig, nil
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
