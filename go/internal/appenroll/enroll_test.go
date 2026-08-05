package appenroll

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/appwire"
)

func newIdentity(t *testing.T) (*Identity, string) {
	t.Helper()
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "nova.key")
	id, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return id, keyPath
}

func TestMintsOnceAndKeepsTheStaticKey(t *testing.T) {
	id, keyPath := newIdentity(t)
	first := id.StaticKey().Public()

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if !bytes.Equal(first, again.StaticKey().Public()) {
		t.Fatal("the static key changed across a restart; every paired phone would be lost")
	}
	if !bytes.Equal(id.RendezvousSecret(), again.RendezvousSecret()) {
		t.Fatal("the rendezvous secret changed across a restart")
	}
}

func TestTheMaterialIsNotWorldReadable(t *testing.T) {
	_, keyPath := newIdentity(t)

	info, err := os.Stat(filepath.Join(filepath.Dir(keyPath), FileName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("mode is %o; the static key is readable by other users", perm)
	}
}

// A known key gets in with no code at all. Without this a phone would need a
// fresh pairing code after every dropped socket, which is not a product.
func TestAKnownKeyNeedsNoPairingCode(t *testing.T) {
	id, _ := newIdentity(t)
	app := appKey(t)

	code, _, err := id.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if err := id.Authorise(app, code); err != nil {
		t.Fatalf("first pairing: %v", err)
	}
	if err := id.Authorise(app, nil); err != nil {
		t.Fatalf("reconnect with no code: %v", err)
	}
}

func TestAnUnknownKeyWithNoCodeIsRefused(t *testing.T) {
	id, _ := newIdentity(t)

	if err := id.Authorise(appKey(t), nil); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("err = %v, want ErrNoPairing", err)
	}
}

// Single use is the property. A photographed QR must not pair a second device
// after the first one has used it.
func TestAPairingCodeIsSpentOnFirstUse(t *testing.T) {
	id, _ := newIdentity(t)

	code, _, err := id.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if err := id.Authorise(appKey(t), code); err != nil {
		t.Fatalf("first use: %v", err)
	}

	second := appKey(t)
	if err := id.Authorise(second, code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("second use: err = %v, want ErrBadPairing", err)
	}
}

func TestAnExpiredCodeIsRefused(t *testing.T) {
	id, _ := newIdentity(t)

	now := time.Now()
	id.now = func() time.Time { return now }
	code, expires, err := id.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if expires.Sub(now) != PairingTTL {
		t.Fatalf("TTL = %v, want %v", expires.Sub(now), PairingTTL)
	}

	id.now = func() time.Time { return now.Add(PairingTTL + time.Second) }
	if err := id.Authorise(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("err = %v, want ErrBadPairing", err)
	}
}

func TestAWrongCodeIsRefused(t *testing.T) {
	id, _ := newIdentity(t)

	if _, _, err := id.MintPairingCode(); err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	wrong := bytes.Repeat([]byte{0x5a}, PairingCodeBytes)
	if err := id.Authorise(appKey(t), wrong); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("err = %v, want ErrBadPairing", err)
	}
}

// Minting a second code must retire the first. Two live codes means two
// strangers can pair, and only one of them was invited.
func TestMintingRetiresThePreviousCode(t *testing.T) {
	id, _ := newIdentity(t)

	first, _, err := id.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if _, _, err := id.MintPairingCode(); err != nil {
		t.Fatalf("second MintPairingCode: %v", err)
	}

	if err := id.Authorise(appKey(t), first); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("err = %v, want ErrBadPairing", err)
	}
}

func TestAuthorisationSurvivesARestart(t *testing.T) {
	id, keyPath := newIdentity(t)
	app := appKey(t)

	code, _, err := id.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if err := id.Authorise(app, code); err != nil {
		t.Fatalf("pairing: %v", err)
	}

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if again.AuthorisedCount() != 1 {
		t.Fatalf("authorised = %d, want 1", again.AuthorisedCount())
	}
	if err := again.Authorise(app, nil); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

func TestRotatingTheRendezvousSecretPersists(t *testing.T) {
	id, keyPath := newIdentity(t)
	before := id.RendezvousSecret()

	after, err := id.RotateRendezvousSecret()
	if err != nil {
		t.Fatalf("RotateRendezvousSecret: %v", err)
	}
	if bytes.Equal(before, after) {
		t.Fatal("rotation returned the same secret")
	}

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if !bytes.Equal(again.RendezvousSecret(), after) {
		t.Fatal("the rotated secret did not survive a restart")
	}
}

// The QR is the app's only trust anchor, so its shape is checked against the
// parser in srcfl/ftw-webapp rather than against this file's own opinion.
func TestEnrollmentURLMatchesTheAppsParser(t *testing.T) {
	id, _ := newIdentity(t)
	code, _, err := id.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	url, err := id.EnrollmentURL(code, "192.168.1.42:8443")
	if err != nil {
		t.Fatalf("EnrollmentURL: %v", err)
	}

	prefix := PayloadOrigin + PayloadPath + "#"
	if !strings.HasPrefix(url, prefix) {
		t.Fatalf("url = %q, want the %q prefix", url, prefix)
	}

	// Every secret after the '#'. A pairing code in a path or a query string
	// would be in a cloud access log before the user finished blinking.
	before, fragment, _ := strings.Cut(url, "#")
	if strings.Contains(before, "?") {
		t.Fatal("the payload has a query string")
	}
	for _, secret := range [][]byte{code, id.RendezvousSecret()} {
		if strings.Contains(before, base64.RawURLEncoding.EncodeToString(secret)) {
			t.Fatal("a secret appears before the fragment")
		}
	}

	parts := strings.Split(fragment, ".")
	if len(parts) != 5 {
		t.Fatalf("%d segments, want 5", len(parts))
	}
	if parts[0] != PayloadVersion {
		t.Fatalf("version = %q, want %q", parts[0], PayloadVersion)
	}

	enc := base64.RawURLEncoding
	for i, want := range []int{0, appwire.DHBytes, PairingCodeBytes, len("192.168.1.42:8443"), RendezvousSecretBytes} {
		if i == 0 {
			continue
		}
		got, err := enc.DecodeString(parts[i])
		if err != nil {
			t.Fatalf("segment %d is not raw base64url: %v", i, err)
		}
		if len(got) != want {
			t.Fatalf("segment %d is %d bytes, want %d", i, len(got), want)
		}
	}
	if !bytes.Equal(mustDecode(t, parts[1]), id.StaticKey().Public()) {
		t.Fatal("segment 1 is not the box's static key")
	}
	if !bytes.Equal(mustDecode(t, parts[4]), id.RendezvousSecret()) {
		t.Fatal("segment 4 is not the rendezvous secret")
	}
}

func TestEnrollmentURLRefusesAHintTheAppWouldReject(t *testing.T) {
	id, _ := newIdentity(t)
	code, _, err := id.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	if _, err := id.EnrollmentURL(code, "192.168.1.1\n:80"); err == nil {
		t.Fatal("a hint with a control character was accepted")
	}
	if _, err := id.EnrollmentURL(code, strings.Repeat("a", MaxLANHintChars+1)); err == nil {
		t.Fatal("an over-long hint was accepted")
	}
}

func TestAnEmptyLANHintIsAllowed(t *testing.T) {
	id, _ := newIdentity(t)
	code, _, err := id.MintPairingCode()
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	url, err := id.EnrollmentURL(code, "")
	if err != nil {
		t.Fatalf("EnrollmentURL: %v", err)
	}
	_, fragment, _ := strings.Cut(url, "#")
	if parts := strings.Split(fragment, "."); len(parts) != 5 || parts[3] != "" {
		t.Fatalf("empty hint did not round trip: %q", url)
	}
}

func TestCorruptMaterialIsAnErrorNotAFreshKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "nova.key")
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte("{"), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// Minting a replacement would unpair the house silently. Failing loudly
	// is the only honest answer to material that exists but cannot be read.
	if _, err := LoadOrCreate(keyPath); err == nil {
		t.Fatal("corrupt material produced a fresh identity")
	}
}

func appKey(t *testing.T) []byte {
	t.Helper()
	pair, err := appwire.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	return pair.Public()
}

func mustDecode(t *testing.T, segment string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatalf("decoding %q: %v", segment, err)
	}
	return raw
}
