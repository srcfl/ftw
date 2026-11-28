package appenroll

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol/appwire"
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

	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if _, err := id.Authorise(app, code); err != nil {
		t.Fatalf("first pairing: %v", err)
	}
	if _, err := id.Authorise(app, nil); err != nil {
		t.Fatalf("reconnect with no code: %v", err)
	}
}

func TestAnUnknownKeyWithNoCodeIsRefused(t *testing.T) {
	id, _ := newIdentity(t)

	if _, err := id.Authorise(appKey(t), nil); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("err = %v, want ErrNoPairing", err)
	}
}

// Single use is the property. A photographed QR must not pair a second device
// after the first one has used it.
func TestAPairingCodeIsSpentOnFirstUse(t *testing.T) {
	id, _ := newIdentity(t)

	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if _, err := id.Authorise(appKey(t), code); err != nil {
		t.Fatalf("first use: %v", err)
	}

	second := appKey(t)
	if _, err := id.Authorise(second, code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("second use: err = %v, want ErrBadPairing", err)
	}
}

func TestAnExpiredCodeIsRefused(t *testing.T) {
	id, _ := newIdentity(t)

	now := time.Now()
	id.now = func() time.Time { return now }
	code, expires, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if expires.Sub(now) != PairingTTL {
		t.Fatalf("TTL = %v, want %v", expires.Sub(now), PairingTTL)
	}

	id.now = func() time.Time { return now.Add(PairingTTL + time.Second) }
	if _, err := id.Authorise(appKey(t), code); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("err = %v, want ErrBadPairing", err)
	}
}

func TestAWrongCodeIsRefused(t *testing.T) {
	id, _ := newIdentity(t)

	if _, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL); err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	wrong := bytes.Repeat([]byte{0x5a}, PairingCodeBytes)
	if _, err := id.Authorise(appKey(t), wrong); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("err = %v, want ErrBadPairing", err)
	}
}

// Minting a second code must retire the first. Two live codes means two
// strangers can pair, and only one of them was invited.
func TestMintingRetiresThePreviousCode(t *testing.T) {
	id, _ := newIdentity(t)

	first, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if _, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL); err != nil {
		t.Fatalf("second MintPairingCode: %v", err)
	}

	if _, err := id.Authorise(appKey(t), first); !errors.Is(err, ErrBadPairing) {
		t.Fatalf("err = %v, want ErrBadPairing", err)
	}
}

func TestAuthorisationSurvivesARestart(t *testing.T) {
	id, keyPath := newIdentity(t)
	app := appKey(t)

	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if _, err := id.Authorise(app, code); err != nil {
		t.Fatalf("pairing: %v", err)
	}

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if again.AuthorisedCount() != 1 {
		t.Fatalf("authorised = %d, want 1", again.AuthorisedCount())
	}
	if _, err := again.Authorise(app, nil); err != nil {
		t.Fatalf("after restart: %v", err)
	}
}

// The QR is the app's only trust anchor, so its shape is checked against the
// parser in srcfl/ftw-webapp rather than against this file's own opinion.
func TestEnrollmentURLMatchesTheAppsParser(t *testing.T) {
	id, _ := newIdentity(t)
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
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
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
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
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
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

// The device list exists so someone can lock a phone out. Everything it
// promises is tested from the outside: rows, order, stamps, and that the old
// bare-string file still reads — an update must never silently unpair a house.
func TestDeviceListAndRevoke(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreate(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}

	base := time.UnixMilli(1_760_000_000_000)
	clock := base
	id.now = func() time.Time { return clock }

	pairPhone := func() []byte {
		code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
		if err != nil {
			t.Fatal(err)
		}
		pub := make([]byte, 32)
		if _, err := rand.Read(pub); err != nil {
			t.Fatal(err)
		}
		if _, err := id.Authorise(pub, code); err != nil {
			t.Fatal(err)
		}
		return pub
	}

	first := pairPhone()
	clock = base.Add(time.Hour)
	second := pairPhone()

	devices := id.Devices()
	if len(devices) != 2 {
		t.Fatalf("%d devices, want 2", len(devices))
	}
	// Most recently seen first: the phone in daily use tops the list, and
	// the stale key someone wants to revoke sinks.
	if devices[0].LastSeenMs != base.Add(time.Hour).UnixMilli() {
		t.Fatalf("newest first: got lastSeen %d", devices[0].LastSeenMs)
	}

	// A reconnect stamps lastSeen — that is what tells a live phone from a
	// key that paired once and vanished.
	clock = base.Add(2 * time.Hour)
	if _, err := id.Authorise(first, nil); err != nil {
		t.Fatalf("reconnect refused: %v", err)
	}
	devices = id.Devices()
	if devices[0].LastSeenMs != clock.UnixMilli() {
		t.Fatal("the reconnect did not stamp lastSeen")
	}

	// Revoke by row id: the key comes back so live sessions can be dropped,
	// and the next handshake meets ErrNoPairing like any stranger's.
	removed, err := id.Revoke(devices[1].ID, OverASession)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || string(removed[0].Key) != string(second) || removed[0].ID != devices[1].ID {
		t.Fatalf("revoke returned %+v, not the one row it removed", removed)
	}
	if _, err := id.Authorise(second, nil); !errors.Is(err, ErrNoPairing) {
		t.Fatalf("a revoked key reconnected: %v", err)
	}
	if _, err := id.Revoke("nosuchid", OverASession); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("revoking a ghost: %v", err)
	}

	// Survives a reload, stamps and all.
	again, err := LoadOrCreate(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatal(err)
	}
	if n := again.AuthorisedCount(); n != 1 {
		t.Fatalf("after reload: %d devices, want 1", n)
	}
}

func TestLegacyBareKeyListStillReads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	// A file written by the previous version: keys as bare strings.
	legacy := `{"noiseSecret":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 32)) +
		`","rendezvousSecret":"` + base64.RawURLEncoding.EncodeToString(make([]byte, 32)) +
		`","authorisedApps":["oldphone-key-in-base64"]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := LoadOrCreate(filepath.Join(dir, "nova.key"))
	if err != nil {
		t.Fatalf("the legacy file must read: %v", err)
	}
	if id.AuthorisedCount() != 1 {
		t.Fatal("the legacy phone was lost in migration")
	}
	devices := id.Devices()
	if len(devices) != 1 || devices[0].AddedAtMs != 0 {
		t.Fatalf("legacy row should carry unknown stamps: %+v", devices)
	}
}

// --------------------------------------------------------------------------
// What a grant says
// --------------------------------------------------------------------------

// The first phone a box pairs is its household's, so it is an owner. A box
// with no owner cannot be administered.
func TestTheFirstPairingIsAnOwner(t *testing.T) {
	id, _ := newIdentity(t)
	code, _, err := id.MintPairingCode(apiauth.RoleOwner, PairingTTL)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	app := appKey(t)
	grant, err := id.Authorise(app, code)
	if err != nil {
		t.Fatalf("pairing: %v", err)
	}
	if grant.Role != apiauth.RoleOwner {
		t.Fatalf("role = %q, want owner", grant.Role)
	}
	if grant.Epoch == 0 {
		t.Fatal("the grant carries no epoch, so a revoke has nothing to move")
	}
	if want := base64.RawURLEncoding.EncodeToString(app)[:8]; grant.DeviceID != want {
		t.Fatalf("device id = %q, want %q", grant.DeviceID, want)
	}

	// The reconnect takes the other branch and must answer the same.
	again, err := id.Authorise(app, nil)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if again != grant {
		t.Fatalf("reconnect granted %+v, want %+v", again, grant)
	}
}

// A box updating from a version with no roles must not silently demote every
// paired phone in the house to viewer.
func TestALegacyEnrolmentLoadsAsAnOwner(t *testing.T) {
	id, keyPath := newIdentity(t)
	app := appKey(t)
	key := base64.RawURLEncoding.EncodeToString(app)

	// applink.json as a box that has never heard of roles wrote it.
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(keyPath), FileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	file["authorisedApps"] = []any{map[string]any{"key": key, "addedAtMs": 1}}
	patched, _ := json.Marshal(file)
	if err := os.WriteFile(filepath.Join(filepath.Dir(keyPath), FileName), patched, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = id

	again, err := LoadOrCreate(keyPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	grant, err := again.Authorise(app, nil)
	if err != nil {
		t.Fatalf("reconnect after the update: %v", err)
	}
	if grant.Role != apiauth.RoleOwner {
		t.Fatalf("role = %q; the update demoted a paired phone", grant.Role)
	}
	if grant.Epoch == 0 {
		t.Fatal("a legacy row came back with no epoch, which no session can match")
	}
}

// Revocation's second layer: the row is gone, so a session still holding an
// epoch has nothing to match against and every privileged request it makes is
// refused before a handler runs.
func TestARevokedDeviceHasNoLiveGrant(t *testing.T) {
	id, _ := newIdentity(t)
	// An owner first, because the only owner cannot be revoked — and a
	// household revoking a phone almost always has another.
	pair(t, id, apiauth.RoleOwner)
	guest := pair(t, id, apiauth.RoleViewer)

	if live, ok := id.GrantFor(guest.DeviceID); !ok || live.Epoch != guest.Epoch {
		t.Fatalf("grant before the revoke = %+v/%v, want %+v/live", live, ok, guest)
	}
	if _, err := id.Revoke(guest.DeviceID, OverASession); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if live, ok := id.GrantFor(guest.DeviceID); ok {
		t.Fatalf("a revoked device still has grant %+v", live)
	}
}
