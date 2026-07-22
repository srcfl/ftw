package nova

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLoadOrCreate_RoundTrip covers the first-run + restart path:
// a fresh call generates + persists a key; a second call on the same
// path loads the identical public key.
func TestLoadOrCreate_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nova.key")
	id1, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if id1.PublicKeyHex() != id2.PublicKeyHex() {
		t.Fatal("public key changed on reload")
	}
	if len(id1.PublicKeyHex()) != 128 {
		t.Fatalf("pubkey hex must be 128 chars (64 bytes X||Y), got %d", len(id1.PublicKeyHex()))
	}
}

func TestLoadOrCreate_ConcurrentFirstWriterWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	const callers = 16
	start := make(chan struct{})
	identities := make(chan *Identity, callers)
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			identity, err := LoadOrCreateIdentity(path)
			identities <- identity
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(identities)
	close(errorsSeen)

	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	persisted, err := loadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	for identity := range identities {
		if identity.PublicKeyHex() != persisted.PublicKeyHex() {
			t.Fatal("concurrent creator returned a key that did not persist")
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "nova.key" {
		t.Fatalf("identity creation left unexpected files: %v", entries)
	}
}

func TestLoadOrCreate_DirectorySyncFailureKeepsInstalledKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nova.key")
	wantErr := errors.New("directory sync failed")
	if _, err := loadOrCreateIdentity(path, func(string) error { return wantErr }); !errors.Is(err, wantErr) {
		t.Fatalf("first create error = %v, want %v", err, wantErr)
	}
	persisted, err := loadIdentity(path)
	if err != nil {
		t.Fatalf("installed key after sync error: %v", err)
	}
	retried, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if retried.PublicKeyHex() != persisted.PublicKeyHex() {
		t.Fatal("retry replaced the key left by the failed directory sync")
	}
}

// TestSignRawHex_VerifiesAsNovaDoes reproduces Nova's
// verifyES256Signature exactly to confirm the wire format is
// byte-compatible (64-byte R||S hex, SHA-256 of message).
func TestSignRawHex_VerifiesAsNovaDoes(t *testing.T) {
	id, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "k.pem"))
	if err != nil {
		t.Fatal(err)
	}
	msg := "idt-op123|nonce-abc|1713610245|gw-f42w-1"
	sigHex, err := id.SignRawHex(msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sigHex) != 128 {
		t.Fatalf("signature must be 128 hex chars (64 bytes R||S), got %d", len(sigHex))
	}

	// Decode pub + sig exactly as Nova's ownership.verifyES256Signature does.
	pubBytes, err := hex.DecodeString(id.PublicKeyHex())
	if err != nil || len(pubBytes) != 64 {
		t.Fatalf("pubkey decode: err=%v len=%d", err, len(pubBytes))
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil || len(sigBytes) != 64 {
		t.Fatalf("sig decode: err=%v len=%d", err, len(sigBytes))
	}
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(pubBytes[:32]),
		Y:     new(big.Int).SetBytes(pubBytes[32:]),
	}
	r := new(big.Int).SetBytes(sigBytes[:32])
	s := new(big.Int).SetBytes(sigBytes[32:])
	hash := sha256.Sum256([]byte(msg))
	if !ecdsa.Verify(pub, hash[:], r, s) {
		t.Fatal("signature did not verify with Nova's verification recipe")
	}
}

// TestSignJWT_FormatMatchesAuthCallout confirms the JWT has three
// base64url segments, ES256 header with device claim, and verifies
// against the identity's own public key.
func TestSignJWT_FormatMatchesAuthCallout(t *testing.T) {
	id, _ := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "k.pem"))
	const serial = "f42w-gw-abc"
	tok, err := id.SignJWT(serial, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT must have 3 parts, got %d", len(parts))
	}

	// Header checks
	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var hdr map[string]string
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		t.Fatal(err)
	}
	if hdr["alg"] != "ES256" {
		t.Fatalf("alg: got %s want ES256", hdr["alg"])
	}
	if hdr["typ"] != "JWT" {
		t.Fatalf("typ: got %s want JWT", hdr["typ"])
	}
	if hdr["device"] != serial {
		t.Fatalf("device claim: got %s want %s", hdr["device"], serial)
	}

	// Payload has iat, exp, jti
	payloadBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var payload map[string]any
	_ = json.Unmarshal(payloadBytes, &payload)
	for _, k := range []string{"iat", "exp", "jti"} {
		if _, ok := payload[k]; !ok {
			t.Fatalf("payload missing %q: %v", k, payload)
		}
	}

	// Verify signature against our own pubkey (mirrors auth-callout's recipe).
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("sig decode: err=%v len=%d", err, len(sig))
	}
	pubBytes, _ := hex.DecodeString(id.PublicKeyHex())
	pub := &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(pubBytes[:32]),
		Y:     new(big.Int).SetBytes(pubBytes[32:]),
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, h[:], r, s) {
		t.Fatal("JWT signature did not verify")
	}
}
