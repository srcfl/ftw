// Package appenroll owns the box's half of app enrollment: the long-lived
// keys behind the QR code on the lid, and the record of which phones have been
// let in.
//
// Three secrets, three different lifetimes, and confusing any two of them is
// the mistake this package exists to prevent:
//
//   - the Noise static key, which the app pins optically and which must never
//     change, because changing it invalidates every pairing at once;
//   - the rendezvous secret, which is long-lived but replaceable, and which
//     only ever feeds the HKDF that names this box on the relay;
//   - the pairing code, which is single-use with a short life and exists only
//     so the box can tell the first handshake from a stranger's.
//
// None of them is stored in SQLite. They are boot-time material that has to be
// readable before the state store is open, and go/internal/state is for site
// data, not for keys.
package appenroll

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/srcfl/ftw/go/internal/appwire"
)

const (
	// PairingCodeBytes matches the app's PAIRING_CODE_BYTES. It is an
	// unguessable token, not a passphrase — nobody types it.
	PairingCodeBytes = 16

	// RendezvousSecretBytes matches the app's RENDEZVOUS_SECRET_BYTES. It is
	// HKDF input keying material, and 32 bytes is what a SHA-256 PRF wants.
	RendezvousSecretBytes = 32

	// PairingTTL is how long a minted code stays usable.
	//
	// Ten minutes is long enough to find the QR code, open the app and hold
	// the phone steady, and short enough that a photograph of the lid taken
	// last year is worth nothing. The code is also spent on first use, so
	// this only bounds the window before anyone uses it at all.
	PairingTTL = 10 * time.Minute

	// FileName is where the material lives, beside nova.key.
	FileName = "applink.json"
)

var (
	// ErrNoPairing is a handshake that offered nothing and came from a key
	// this box has never authorised.
	ErrNoPairing = errors.New("appenroll: no pairing code and an unknown app key")
	// ErrBadPairing is a code that is wrong, expired or already spent.
	ErrBadPairing = errors.New("appenroll: pairing code is not valid")
)

// stored is the on-disk shape. Base64 rather than hex only because the app's
// QR payload is base64url and one encoding across the pair is one less thing
// to get wrong.
type stored struct {
	NoiseSecret      string `json:"noiseSecret"`
	RendezvousSecret string `json:"rendezvousSecret"`
	// AuthorisedApps holds every phone that has completed a pairing, with
	// enough metadata to tell them apart on the settings page. A key here
	// needs no code on later handshakes, which is what makes the code
	// single-use without breaking reconnects.
	//
	// Earlier versions stored a bare list of key strings; load() reads both
	// so an update never silently unpairs a house.
	AuthorisedApps []storedApp `json:"authorisedApps"`
}

type storedApp struct {
	Key        string `json:"key"`
	AddedAtMs  int64  `json:"addedAtMs,omitempty"`
	LastSeenMs int64  `json:"lastSeenMs,omitempty"`
}

// UnmarshalJSON accepts the legacy bare-string form beside the current one.
func (a *storedApp) UnmarshalJSON(raw []byte) error {
	var key string
	if err := json.Unmarshal(raw, &key); err == nil {
		*a = storedApp{Key: key}
		return nil
	}
	type plain storedApp
	var v plain
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*a = storedApp(v)
	return nil
}

// DeviceInfo is one paired phone, as the settings page needs to see it.
//
// The ID is a prefix of the key, not the key: enough to name a row and to
// revoke it, not enough to impersonate anyone even in a leaked screenshot.
type DeviceInfo struct {
	ID         string
	AddedAtMs  int64
	LastSeenMs int64
}

// deviceID is the row name: the first eight characters of the base64 key.
func deviceID(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:8]
}

// Identity is the box's app-facing enrollment state.
//
// Safe for concurrent use: the uplink authorises handshakes on its own
// goroutine while the API may be minting a code for the screen.
type Identity struct {
	path string

	mu     sync.Mutex
	static appwire.KeyPair
	// staticSecret is kept beside the key pair because appwire deliberately
	// exposes only the public half; persisting the private one is this
	// package's job and nobody else's.
	staticSecret []byte
	rendezvous   []byte
	authorised   map[string]*appMeta
	pairing      *pairingCode
	now          func() time.Time
	// randRead is the entropy source. Injectable so a test can prove the
	// failure path instead of only the happy one.
	randRead func([]byte) (int, error)
}

// appMeta is what the box remembers about one paired phone.
type appMeta struct {
	addedAtMs  int64
	lastSeenMs int64
}

type pairingCode struct {
	code      []byte
	expiresAt time.Time
}

// LoadOrCreate reads the material beside keyPath, minting whatever is missing.
//
// Minting is idempotent in the sense that matters: an existing static key is
// never replaced. Replacing it would silently unpair every phone in the house,
// and a box that has forgotten who it is should say so rather than quietly
// become a different box.
func LoadOrCreate(keyPath string) (*Identity, error) {
	path := filepath.Join(filepath.Dir(keyPath), FileName)

	id := &Identity{
		path:       path,
		authorised: map[string]*appMeta{},
		now:        time.Now,
		randRead:   rand.Read,
	}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := id.load(raw); err != nil {
			return nil, err
		}
		return id, nil
	case errors.Is(err, os.ErrNotExist):
		if err := id.mint(); err != nil {
			return nil, err
		}
		return id, id.save()
	default:
		return nil, fmt.Errorf("appenroll: reading %s: %w", path, err)
	}
}

func (i *Identity) load(raw []byte) error {
	var s stored
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("appenroll: %s is not readable: %w", i.path, err)
	}

	secret, err := base64.RawURLEncoding.DecodeString(s.NoiseSecret)
	if err != nil {
		return fmt.Errorf("appenroll: static key in %s is not base64: %w", i.path, err)
	}
	static, err := appwire.KeyPairFromSecret(secret)
	if err != nil {
		return fmt.Errorf("appenroll: static key in %s: %w", i.path, err)
	}
	rendezvous, err := base64.RawURLEncoding.DecodeString(s.RendezvousSecret)
	if err != nil {
		return fmt.Errorf("appenroll: rendezvous secret in %s is not base64: %w", i.path, err)
	}
	if len(rendezvous) != RendezvousSecretBytes {
		return fmt.Errorf("appenroll: rendezvous secret in %s is %d bytes, need %d",
			i.path, len(rendezvous), RendezvousSecretBytes)
	}

	i.static = static
	i.staticSecret = secret
	i.rendezvous = rendezvous
	for _, app := range s.AuthorisedApps {
		i.authorised[app.Key] = &appMeta{addedAtMs: app.AddedAtMs, lastSeenMs: app.LastSeenMs}
	}
	return nil
}

func (i *Identity) mint() error {
	// Drawn here rather than by appwire.GenerateKeyPair because the secret
	// has to be written to disk, and appwire never hands one back — a key
	// that cannot be exported is the right default for a crypto package.
	secret := make([]byte, appwire.DHBytes)
	if _, err := i.randRead(secret); err != nil {
		return fmt.Errorf("appenroll: minting the static key: %w", err)
	}
	static, err := appwire.KeyPairFromSecret(secret)
	if err != nil {
		return fmt.Errorf("appenroll: minting the static key: %w", err)
	}
	rendezvous := make([]byte, RendezvousSecretBytes)
	if _, err := i.randRead(rendezvous); err != nil {
		return fmt.Errorf("appenroll: minting the rendezvous secret: %w", err)
	}

	i.static = static
	i.staticSecret = secret
	i.rendezvous = rendezvous
	return nil
}

// save writes through a temporary file and a rename, so a power cut during the
// write leaves the old material rather than half of the new. A box that loses
// its static key is a box every phone in the house has to be re-paired with.
func (i *Identity) save() error {
	i.mu.Lock()
	s := stored{
		NoiseSecret:      base64.RawURLEncoding.EncodeToString(i.staticSecret),
		RendezvousSecret: base64.RawURLEncoding.EncodeToString(i.rendezvous),
		AuthorisedApps:   make([]storedApp, 0, len(i.authorised)),
	}
	for key, meta := range i.authorised {
		s.AuthorisedApps = append(s.AuthorisedApps, storedApp{
			Key: key, AddedAtMs: meta.addedAtMs, LastSeenMs: meta.lastSeenMs,
		})
	}
	// Deterministic on disk, so two saves of the same state are the same
	// bytes and a diff of applink.json says something.
	sort.Slice(s.AuthorisedApps, func(a, b int) bool {
		return s.AuthorisedApps[a].Key < s.AuthorisedApps[b].Key
	})
	i.mu.Unlock()

	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("appenroll: encoding %s: %w", i.path, err)
	}

	tmp := i.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("appenroll: writing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, i.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("appenroll: replacing %s: %w", i.path, err)
	}
	return nil
}

// StaticKey is the box's Noise static key pair.
func (i *Identity) StaticKey() appwire.KeyPair {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.static
}

// RendezvousSecret is the HKDF input the relay handle is derived from.
func (i *Identity) RendezvousSecret() []byte {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]byte(nil), i.rendezvous...)
}

// RotateRendezvousSecret replaces the secret and returns the new one.
//
// Every paired phone has to be told the new value before it can find this box
// again, so the caller owns that half. Provided because the alternative — a
// secret fixed for the life of the box — is the thing hourly rotation exists
// to avoid, one level up.
func (i *Identity) RotateRendezvousSecret() ([]byte, error) {
	secret := make([]byte, RendezvousSecretBytes)
	if _, err := i.randRead(secret); err != nil {
		return nil, fmt.Errorf("appenroll: minting a rendezvous secret: %w", err)
	}

	i.mu.Lock()
	i.rendezvous = secret
	i.mu.Unlock()

	if err := i.save(); err != nil {
		return nil, err
	}
	return append([]byte(nil), secret...), nil
}

// MintPairingCode issues a fresh single-use code and forgets any previous one.
//
// Forgetting matters: two live codes means two strangers can pair, and the
// second one was minted by whoever pressed the button last. One code at a time
// is the whole safety property.
func (i *Identity) MintPairingCode() ([]byte, time.Time, error) {
	code := make([]byte, PairingCodeBytes)
	if _, err := i.randRead(code); err != nil {
		return nil, time.Time{}, fmt.Errorf("appenroll: minting a pairing code: %w", err)
	}
	expires := i.now().Add(PairingTTL)

	i.mu.Lock()
	i.pairing = &pairingCode{code: code, expiresAt: expires}
	i.mu.Unlock()

	return append([]byte(nil), code...), expires, nil
}

// Authorise decides whether a finished handshake may become a session.
//
// appStatic is the app's static key as the handshake authenticated it, never
// as it was claimed; payload is handshake message 1's plaintext.
//
// A key already on the list needs no code, which is what lets a phone
// reconnect after every dropped socket without the box handing out a second
// pairing code. Anything else needs a live code, and spending one records the
// key so the next reconnect takes the first branch.
func (i *Identity) Authorise(appStatic, payload []byte) error {
	key := base64.RawURLEncoding.EncodeToString(appStatic)

	i.mu.Lock()
	if meta := i.authorised[key]; meta != nil {
		// A reconnect. The stamp is what lets the settings page tell a
		// phone in daily use from a key that paired once and vanished —
		// which is exactly the row someone wants to revoke.
		meta.lastSeenMs = i.now().UnixMilli()
		i.mu.Unlock()
		if err := i.save(); err != nil {
			// A stamp that could not be persisted must not block a session.
			return nil
		}
		return nil
	}
	pairing := i.pairing
	i.mu.Unlock()

	if len(payload) == 0 {
		return ErrNoPairing
	}
	if pairing == nil || !i.now().Before(pairing.expiresAt) {
		return ErrBadPairing
	}
	// Constant time, because a byte-at-a-time comparison against a live code
	// is exactly the oracle an attacker with a relay socket would want.
	if subtle.ConstantTimeCompare(payload, pairing.code) != 1 {
		return ErrBadPairing
	}

	i.mu.Lock()
	now := i.now().UnixMilli()
	i.authorised[key] = &appMeta{addedAtMs: now, lastSeenMs: now}
	// Spent. A second phone offering the same photographed code now meets
	// ErrBadPairing rather than a pairing.
	i.pairing = nil
	i.mu.Unlock()

	return i.save()
}

// Devices lists every paired phone, most recently seen first.
func (i *Identity) Devices() []DeviceInfo {
	i.mu.Lock()
	out := make([]DeviceInfo, 0, len(i.authorised))
	for key, meta := range i.authorised {
		out = append(out, DeviceInfo{
			ID: deviceID(key), AddedAtMs: meta.addedAtMs, LastSeenMs: meta.lastSeenMs,
		})
	}
	i.mu.Unlock()

	sort.Slice(out, func(a, b int) bool {
		if out[a].LastSeenMs != out[b].LastSeenMs {
			return out[a].LastSeenMs > out[b].LastSeenMs
		}
		return out[a].ID < out[b].ID
	})
	return out
}

// ErrUnknownDevice is a revoke aimed at an id no paired phone carries.
var ErrUnknownDevice = errors.New("appenroll: no such device")

// Revoke forgets a phone by its device id and returns the full key, so the
// caller can also tear down any session that key is running right now. The
// next handshake from it meets ErrNoPairing like any stranger's.
func (i *Identity) Revoke(id string) ([]byte, error) {
	i.mu.Lock()
	var fullKey string
	for key := range i.authorised {
		if deviceID(key) == id {
			fullKey = key
			break
		}
	}
	if fullKey == "" {
		i.mu.Unlock()
		return nil, ErrUnknownDevice
	}
	delete(i.authorised, fullKey)
	i.mu.Unlock()

	if err := i.save(); err != nil {
		return nil, err
	}
	raw, err := base64.RawURLEncoding.DecodeString(fullKey)
	if err != nil {
		// The key was minted by EncodeToString; this cannot happen outside a
		// corrupted file, and the revoke itself already stuck.
		return nil, nil
	}
	return raw, nil
}

// AuthorisedCount is for the API and for tests. The keys themselves stay here.
func (i *Identity) AuthorisedCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.authorised)
}
