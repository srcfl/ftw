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

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/protocol"
	"github.com/srcfl/ftw/go/protocol/appwire"
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

	// InviteTTL is how long a shared, view-only code stays usable.
	//
	// The same ten minutes as an owner's, and deliberately not dressed up as
	// less. An invite is not a weaker kind of code — it is the same code
	// carrying a different role, and shortening its life would suggest the
	// household is protected by the clock when what protects them is the
	// role the box stamps at redemption.
	InviteTTL = PairingTTL

	// SpokenTTL is how long a box code stays usable.
	//
	// Half an owner code's life, and this one is a real difference: forty
	// bits read down a phone is less entropy than sixteen random bytes
	// scanned off a screen, and it is read out while both people are on the
	// line. Five minutes is the length of that conversation.
	SpokenTTL = 5 * time.Minute

	// MaxPairingAttempts is how many wrong guesses a code survives.
	//
	// This is the number that makes a spoken code sound out loud: forty bits
	// with unlimited tries is a lock somebody can pick given a weekend, and
	// forty bits with five tries is one nobody can pick at all. See the
	// counter in Authorise for why it is on the code and not on the caller.
	MaxPairingAttempts = 5

	// FileName is where the material lives, beside nova.key.
	FileName = "applink.json"
)

var (
	// ErrNoPairing is a handshake that offered nothing and came from a key
	// this box has never authorised.
	ErrNoPairing = errors.New("appenroll: no pairing code and an unknown app key")
	// ErrBadPairing is a code that is wrong, expired, already spent or has
	// been guessed at too many times. One error for all four on purpose: a
	// guesser who could tell "wrong" from "expired" would learn when to stop
	// wasting attempts, and none of the four is a different sentence to the
	// household — they all mean ask for a new code.
	ErrBadPairing = errors.New("appenroll: pairing code is not valid")
	// ErrUnknownRole is a role that is not in contract/registry.yaml.
	ErrUnknownRole = errors.New("appenroll: no such role")
	// ErrLastOwnerProtected is an attempt to demote the only owner, or to
	// remove it from somewhere other than the box itself.
	ErrLastOwnerProtected = errors.New("appenroll: that is the only owner")
	// ErrWatchRole is the watch role asked for where it cannot be given: on
	// a phone's code, or as a role change to or from watch. A watch comes
	// only from an owner phone's watch code and stays a watch until removed.
	ErrWatchRole = errors.New("appenroll: a watch comes only from an owner phone's watch code")
	// ErrWatchNeedsOwner is a watch code asked for by a phone that is not an
	// owner. A watch never holds more than its phone, and only an owner may
	// add a device.
	ErrWatchNeedsOwner = errors.New("appenroll: only an owner phone can add a watch")
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
	AuthorisedApps []storedApp    `json:"authorisedApps"`
	AgentInvites   []storedInvite `json:"agentInvites,omitempty"`
}

type storedApp struct {
	Key        string `json:"key"`
	AddedAtMs  int64  `json:"addedAtMs,omitempty"`
	LastSeenMs int64  `json:"lastSeenMs,omitempty"`
	// Role is what this phone may do. Absent on a row written before roles
	// existed, and load() reads that as owner: a box updating from a version
	// with no roles must not silently demote every paired phone to viewer.
	Role string `json:"role,omitempty"`
	// Epoch counts how many times this grant has been given a different
	// role. It is what a log line names to say which version of a grant a
	// request ran under; it is not what makes a change bite. That is
	// GrantFor, which every privileged request calls. Absent, or zero, reads
	// as the first epoch.
	Epoch uint64 `json:"epoch,omitempty"`
	// Parent is the full key of the phone that added this watch. Empty for a
	// phone. A watch without its phone is removed with it, never left behind.
	Parent string `json:"parent,omitempty"`
	// Agent is an agent row's grant (agents.go). Nil for a phone or a watch.
	Agent *storedAgent `json:"agent,omitempty"`
	// ApprovalKey is an owner phone's P-256 approval key, SEC1 uncompressed
	// in base64url, and ApprovalKeySetMs when it was registered.
	ApprovalKey      string `json:"approvalKey,omitempty"`
	ApprovalKeySetMs int64  `json:"approvalKeySetMs,omitempty"`
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
	// Role is what this phone may do. It belongs in the list because sharing
	// and locking out are the same screen: a household that cannot see which
	// phone is a guest cannot decide which one to remove.
	Role string
	// LastOwner marks the only phone left that can change anything here. It
	// cannot be stepped down at either door, and cannot be removed over a
	// session — so a screen can say what it is before somebody presses a
	// button rather than after.
	LastOwner bool
	// Parent is the device id of the phone that added this watch, and empty
	// for a phone. The list puts each watch right after its phone.
	Parent string
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

	saveMu sync.Mutex
	mu     sync.Mutex
	static appwire.KeyPair
	// staticSecret is kept beside the key pair because appwire deliberately
	// exposes only the public half; persisting the private one is this
	// package's job and nobody else's.
	staticSecret      []byte
	rendezvous        []byte
	authorised        map[string]*appMeta
	invites           map[string]storedInvite
	offerKeys         map[string][]byte
	prepareInviteHome func() (string, error)
	onInviteClaimed   func(protocol.AgentInvite, protocol.AgentRow)
	pairing           *pairingCode
	now               func() time.Time
	// randRead is the entropy source. Injectable so a test can prove the
	// failure path instead of only the happy one.
	randRead func([]byte) (int, error)
	// onChange hears about every change to who is paired and what they
	// may do, after it is saved. Not about lastSeen stamps.
	onChange []func()
	// onPairingCode hears about a phone's code minted, which puts a new
	// code on the screen or takes one off it.
	onPairingCode []func()
	// home is the home id at FTW Cloud, for a QR that names it. See
	// SetHome.
	home func() string
	// wall is the wall clock and whether NTP has set it. Agent rows expire
	// by it and may do nothing without it. See SetClock.
	wall func() (time.Time, bool)
	// seenApprovals remembers the SHA-256 of each signed array it took
	// until the ms it maps to, so none is taken twice.
	seenApprovals map[[32]byte]int64
	// sinceMs is the earliest approval time this process takes: its first
	// trusted wall time less the skew allowed.
	sinceMs int64
	// onOwnerAdded hears about a phone paired as an owner.
	onOwnerAdded []func()
}

// appMeta is what the box remembers about one paired phone.
type appMeta struct {
	addedAtMs  int64
	lastSeenMs int64
	role       string
	epoch      uint64
	// parent is the full key of the phone that added a watch, and empty for
	// a phone.
	parent string
	// agent is an agent row's grant, nil for a phone or a watch.
	agent *agentMeta
	// approvalKey is an owner phone's approval key, empty when it has none.
	approvalKey      []byte
	approvalKeySetMs int64
}

// firstEpoch is where a fresh enrolment starts.
//
// One and not zero, so a row loaded from a file written before epochs existed
// is the first epoch rather than an unwritten field.
const firstEpoch uint64 = 1

// Grant is what a finished handshake earned: which device, what it may do,
// and which version of that answer it was told.
type Grant struct {
	DeviceID string
	Role     string
	Epoch    uint64
	// Parent and ParentRole name the phone that added a watch and the role
	// that phone holds now. Both are empty for a phone. A watch holds only
	// the scopes its parent's role also carries, so a parent demoted to
	// viewer narrows its watch at once.
	Parent     string
	ParentRole string
	// Agent is an agent row's grant now, nil for a phone or a watch. Its
	// ParentRole is the role its approver holds now: owner for a grant
	// made on the box page.
	Agent *AgentGrant
}

type pairingCode struct {
	code      []byte
	expiresAt time.Time
	// role is what the code lets in. Held by the box and stamped when the
	// code is spent, never carried in the QR payload: a role in the fragment
	// is a claim its holder can edit.
	role string
	// kind is "qr" or "spoken", and it exists to be logged and shown rather
	// than to be checked. Both kinds are redeemed identically — the same
	// field of the same handshake message — and the only thing that differs
	// is how many bytes were drawn and how long they last.
	kind string
	// attempts counts wrong guesses against this code. See Authorise.
	attempts int
}

// Kinds of pairing code.
const (
	// PairingKindQR is sixteen bytes, scanned off a screen.
	PairingKindQR = "qr"
	// PairingKindSpoken is five bytes, read aloud. See boxcode.go.
	PairingKindSpoken = "spoken"
)

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
		invites:    map[string]storedInvite{},
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
	for _, inv := range s.AgentInvites {
		i.invites[inv.ID] = inv
	}
	for _, app := range s.AuthorisedApps {
		role := app.Role
		if role == "" {
			// A row from before roles existed. Owner, because these are the
			// phones the household already paired and trusts; reading them
			// as viewers would lock a house out of its own box on an update.
			role = apiauth.RoleOwner
		}
		epoch := app.Epoch
		if epoch == 0 {
			epoch = firstEpoch
		}
		meta := &appMeta{
			addedAtMs:        app.AddedAtMs,
			lastSeenMs:       app.LastSeenMs,
			role:             role,
			epoch:            epoch,
			parent:           app.Parent,
			agent:            app.Agent,
			approvalKeySetMs: app.ApprovalKeySetMs,
		}
		if app.ApprovalKey != "" {
			meta.approvalKey, _ = base64.RawURLEncoding.DecodeString(app.ApprovalKey)
		}
		i.authorised[app.Key] = meta
	}
	// A watch whose phone is missing from the file was left behind by a
	// write that never finished. It holds nothing its phone could vouch for,
	// so it goes rather than lingering as a device nobody can narrow.
	for key, meta := range i.authorised {
		if meta.role == apiauth.RoleWatch && i.authorised[meta.parent] == nil {
			delete(i.authorised, key)
		}
	}
	// The same for an agent whose approving phone is gone, and for a row
	// whose role says agent and carries no grant.
	for key, meta := range i.authorised {
		agentRole := apiauth.AgentRole(meta.role)
		switch {
		case agentRole != (meta.agent != nil):
			delete(i.authorised, key)
		case meta.agent != nil && meta.agent.ApprovedBy != LANApprover && i.authorised[meta.agent.ApprovedBy] == nil:
			delete(i.authorised, key)
		}
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
	i.saveMu.Lock()
	defer i.saveMu.Unlock()
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.persistLocked(i.snapshotLocked())
}

// The caller holds saveMu then mu through persistence and publication.
func (i *Identity) snapshotLocked() stored {
	s := stored{
		NoiseSecret:      base64.RawURLEncoding.EncodeToString(i.staticSecret),
		RendezvousSecret: base64.RawURLEncoding.EncodeToString(i.rendezvous),
		AuthorisedApps:   make([]storedApp, 0, len(i.authorised)),
	}
	for key, meta := range i.authorised {
		app := storedApp{
			Key: key, AddedAtMs: meta.addedAtMs, LastSeenMs: meta.lastSeenMs,
			Role: meta.role, Epoch: meta.epoch, Parent: meta.parent, Agent: meta.agent,
			ApprovalKeySetMs: meta.approvalKeySetMs,
		}
		if len(meta.approvalKey) > 0 {
			app.ApprovalKey = base64.RawURLEncoding.EncodeToString(meta.approvalKey)
		}
		s.AuthorisedApps = append(s.AuthorisedApps, app)
	}
	// Deterministic on disk, so two saves of the same state are the same
	// bytes and a diff of applink.json says something.
	sort.Slice(s.AuthorisedApps, func(a, b int) bool {
		return s.AuthorisedApps[a].Key < s.AuthorisedApps[b].Key
	})
	for _, inv := range i.invites {
		s.AgentInvites = append(s.AgentInvites, inv)
	}
	sort.Slice(s.AgentInvites, func(a, b int) bool { return s.AgentInvites[a].ID < s.AgentInvites[b].ID })
	return s
}

func (i *Identity) persistLocked(s stored) error {
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
// is the whole safety property, and it holds across kinds — minting an invite
// cancels a pending owner code and the reverse. The screen already copes with
// a code expiring, so it copes with this.
//
// The role is remembered here rather than carried in the QR payload, because a
// role the holder can edit is not a role. Nothing about the payload changes
// between an owner's code and a viewer's: the box remembers which it minted
// and stamps it when the code is spent.
//
// What stops the code being replayed, in the order it stops it:
//   - it is spent at the first success, so the second presentation of a
//     photographed code finds no live code at all;
//   - it lives for ttl and no longer;
//   - it survives MaxPairingAttempts wrong guesses and is then burned;
//   - it rides inside Noise handshake message 1, encrypted to this box's
//     static key, so the relay carrying it never sees it — and a replay of
//     that whole message earns nothing, because the replayer holds neither
//     the initiator's ephemeral nor its static private key and so cannot
//     derive the session keys the box answers with.
func (i *Identity) MintPairingCode(role string, ttl time.Duration) ([]byte, time.Time, error) {
	if err := phoneRole(role); err != nil {
		return nil, time.Time{}, err
	}
	code := make([]byte, PairingCodeBytes)
	if _, err := i.randRead(code); err != nil {
		return nil, time.Time{}, fmt.Errorf("appenroll: minting a pairing code: %w", err)
	}
	expires := i.now().Add(ttl)

	i.mu.Lock()
	i.pairing = &pairingCode{code: code, expiresAt: expires, role: role, kind: PairingKindQR}
	i.mu.Unlock()
	i.minted()

	return append([]byte(nil), code...), expires, nil
}

// MintSpokenCode issues a box code: eight characters somebody can read aloud.
//
// It replaces any live code, exactly as MintPairingCode does, and it is spent,
// timed and counted by the same machinery. The only differences are that it
// carries forty bits rather than a hundred and twenty-eight, and that it lives
// for five minutes rather than ten.
//
// Returned grouped as XXXX-XXXX, ready to be shown and read. The bytes are
// what travels; the characters are for the person.
func (i *Identity) MintSpokenCode(role string) (string, time.Time, error) {
	if err := phoneRole(role); err != nil {
		return "", time.Time{}, err
	}
	code := make([]byte, SpokenCodeBytes)
	if _, err := i.randRead(code); err != nil {
		return "", time.Time{}, fmt.Errorf("appenroll: minting a box code: %w", err)
	}
	expires := i.now().Add(SpokenTTL)

	i.mu.Lock()
	i.pairing = &pairingCode{code: code, expiresAt: expires, role: role, kind: PairingKindSpoken}
	i.mu.Unlock()
	i.minted()

	return GroupSpokenCode(encodeSpoken(code)), expires, nil
}

// PendingCode is the QR code a phone may pair with now, for FTW Cloud's
// roster to list by its hash, so the service admits that phone's first
// session. A spoken code is never one: forty bits behind a hash are forty
// bits the service could find. Nor is a watch code, an expired code or a
// spent one.
func (i *Identity) PendingCode() ([]byte, time.Time, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	p := i.pairing
	if p == nil || p.kind != PairingKindQR || !i.now().Before(p.expiresAt) {
		return nil, time.Time{}, false
	}
	return append([]byte(nil), p.code...), p.expiresAt, true
}

// OnPairingCode registers fn to run after a phone's code is minted, which
// shows a new one and takes any older one off the screen. A code spent is
// a pairing, and OnChange hears about it.
func (i *Identity) OnPairingCode(fn func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.onPairingCode = append(i.onPairingCode, fn)
}

func (i *Identity) minted() {
	i.mu.Lock()
	fns := append([]func(){}, i.onPairingCode...)
	i.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// knownRole refuses a role the registry does not define.
//
// Checked at every door into the stored state rather than trusted from the
// caller, because a role nobody recognises expands to no scopes at all — so a
// typo would not fail loudly, it would quietly make a phone that can do
// nothing and give nobody a reason why.
func knownRole(role string) error {
	if _, ok := apiauth.RoleScopes[role]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownRole, role)
	}
	return nil
}

// phoneRole refuses a role a phone cannot hold: one the registry does not
// define, watch, which only an owner's phone hands out by key (AddWatch), and an agent's, which
// only an owner's signed grant hands out.
func phoneRole(role string) error {
	if err := knownRole(role); err != nil {
		return err
	}
	if role == apiauth.RoleWatch {
		return ErrWatchRole
	}
	if apiauth.AgentRole(role) {
		return fmt.Errorf("%w: %q is an agent's role", ErrUnknownRole, role)
	}
	return nil
}

// Authorise decides whether a finished handshake may become a session, and
// says what the session may do.
//
// appStatic is the app's static key as the handshake authenticated it, never
// as it was claimed; payload is handshake message 1's plaintext.
//
// A key already on the list needs no code, which is what lets a phone
// reconnect after every dropped socket without the box handing out a second
// pairing code. Anything else needs a live code, and spending one records the
// key so the next reconnect takes the first branch.
//
// The Grant is the seam. This function has always known which device it just
// authenticated; until now it threw that away and answered only yes or no,
// which is why nothing downstream could tell two phones apart.
func (i *Identity) Authorise(appStatic, payload []byte) (Grant, error) {
	_, commit, err := i.Check(appStatic, payload)
	if err != nil {
		return Grant{}, err
	}
	return commit()
}

// Check is Authorise in two steps: it decides now, and changes nothing but a
// wrong guess's count until commit runs. A carrier commits once the session
// has proved itself with its first transport message, so a handshake message
// replayed by whoever carries it costs a reconnect stamp nothing, spends no
// code and takes no device's place. commit returns the grant as it stands
// then, and fails if a revoke, another pairing or the code's end came first.
func (i *Identity) Check(appStatic, payload []byte) (Grant, func() (Grant, error), error) {
	return i.check(appStatic, payload, false)
}

// CheckTunnel is Check for a session FTW Cloud carries. It also admits an
// agent row that has a clock to run by and has not expired: an agent reaches
// the box through the tunnel and nowhere else.
func (i *Identity) CheckTunnel(appStatic, payload []byte) (Grant, func() (Grant, error), error) {
	if protocol.IsAgentInvitePayload(payload) {
		return i.checkAgentInvite(appStatic, payload)
	}
	return i.check(appStatic, payload, true)
}

func (i *Identity) check(appStatic, payload []byte, tunnel bool) (Grant, func() (Grant, error), error) {
	key := base64.RawURLEncoding.EncodeToString(appStatic)

	i.mu.Lock()
	if meta := i.authorised[key]; meta != nil {
		if meta.role == apiauth.RoleWatch {
			// A watch has no stream. It reaches the box through the
			// exchange, which never takes a phone's place, and
			// it answers here as a stranger would.
			i.mu.Unlock()
			return Grant{}, nil, ErrNoPairing
		}
		if meta.agent != nil {
			// An agent comes only through the tunnel, only with a
			// clock, and only before its grant runs out. Anywhere else
			// it is a stranger.
			if g, _, ok := i.agentGrantLocked(meta); !tunnel || !ok || g.Expired || g.NoClock {
				i.mu.Unlock()
				return Grant{}, nil, ErrNoPairing
			}
		}
		grant := i.grantLocked(key, meta)
		i.mu.Unlock()
		// A reconnect. The stamp is what lets the settings page tell a
		// phone in daily use from a key that paired once and vanished —
		// which is exactly the row someone wants to revoke.
		return grant, func() (Grant, error) {
			i.mu.Lock()
			meta := i.authorised[key]
			if meta == nil {
				i.mu.Unlock()
				return Grant{}, ErrNoPairing
			}
			meta.lastSeenMs = i.now().UnixMilli()
			grant := i.grantLocked(key, meta)
			i.mu.Unlock()
			// A stamp that could not be persisted must not block a session.
			_ = i.save()
			return grant, nil
		}, nil
	}
	// The rest happens under the lock, because counting a wrong guess is a
	// read and a write of the same code and two handshakes racing must not
	// each see four attempts and let a fifth and a sixth through.
	if len(payload) == 0 {
		i.mu.Unlock()
		return Grant{}, nil, ErrNoPairing
	}
	pairing := i.pairing
	if pairing == nil || !i.now().Before(pairing.expiresAt) {
		i.mu.Unlock()
		return Grant{}, nil, ErrBadPairing
	}
	// Constant time, because a byte-at-a-time comparison against a live code
	// is exactly the oracle an attacker with a relay socket would want.
	if subtle.ConstantTimeCompare(payload, pairing.code) != 1 {
		// A failure counter on the code itself, and this is the layer that
		// makes forty bits safe to say out loud: a guesser gets five tries
		// per minting, and every minting needs a person standing at the box.
		//
		// Deliberately not keyed on the caller's address. docs/architecture.md
		// records that the relay's own limiter keys on the socket address,
		// which behind the documented TLS terminator is a single address for
		// the whole fleet — an address-keyed counter here would inherit that
		// bug and count the world as one guesser.
		//
		// The cost is real and accepted: anyone who can reach this box can
		// burn a live code by guessing at it, and the household has to ask
		// for another. Denying somebody a code they can re-mint in a second
		// is a far smaller harm than a code that can be ground down at
		// leisure.
		pairing.attempts++
		if pairing.attempts >= MaxPairingAttempts {
			i.pairing = nil
		}
		i.mu.Unlock()
		return Grant{}, nil, ErrBadPairing
	}
	grant := Grant{DeviceID: deviceID(key), Role: i.pairingRoleLocked(pairing), Epoch: firstEpoch}
	i.mu.Unlock()

	return grant, func() (Grant, error) {
		i.mu.Lock()
		if meta := i.authorised[key]; meta != nil {
			// The same phone's other session committed first.
			grant := i.grantLocked(key, meta)
			i.mu.Unlock()
			return grant, nil
		}
		// The code must still be the one this handshake carried: not spent
		// by another phone, replaced or out of time.
		if i.pairing != pairing || !i.now().Before(pairing.expiresAt) {
			i.mu.Unlock()
			return Grant{}, ErrBadPairing
		}
		now := i.now().UnixMilli()
		role := i.pairingRoleLocked(pairing)
		i.authorised[key] = &appMeta{addedAtMs: now, lastSeenMs: now, role: role, epoch: firstEpoch}
		// Spent. A second phone offering the same photographed code now meets
		// ErrBadPairing rather than a pairing.
		i.pairing = nil
		i.mu.Unlock()

		err := i.save()
		i.changed()
		if role == apiauth.RoleOwner {
			i.ownerAdded()
		}
		return Grant{DeviceID: deviceID(key), Role: role, Epoch: firstEpoch}, err
	}, nil
}

// OnOwnerAdded registers fn to run after a phone pairs as an owner: every
// owner hears of it.
func (i *Identity) OnOwnerAdded(fn func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.onOwnerAdded = append(i.onOwnerAdded, fn)
}

func (i *Identity) ownerAdded() {
	i.mu.Lock()
	fns := append([]func(){}, i.onOwnerAdded...)
	i.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// pairingRoleLocked is the role a code stamps. The role travels with the code
// rather than with the handshake, so a code minted for a viewer stamps a
// viewer, and nothing the app sends can change it. The caller holds i.mu.
func (i *Identity) pairingRoleLocked(pairing *pairingCode) string {
	role := pairing.role
	if role == "" {
		role = apiauth.RoleOwner
	}
	if role != apiauth.RoleOwner && !i.hasOwnerLocked() {
		// Whoever creates a home owns it. The first enrolment is an owner
		// whatever its code said, because a box with no owner cannot be
		// administered by anybody, from anywhere, ever again — and a
		// household that handed out a view-only code before pairing its own
		// phone would have built exactly that.
		role = apiauth.RoleOwner
	}
	return role
}

// hasOwnerLocked reports whether any enrolment can still administer this box.
// The caller holds the mutex.
func (i *Identity) hasOwnerLocked() bool {
	for _, meta := range i.authorised {
		if meta.role == apiauth.RoleOwner {
			return true
		}
	}
	return false
}

// ownerCountLocked is how many enrolments can administer this box. The caller
// holds the mutex.
func (i *Identity) ownerCountLocked() int {
	n := 0
	for _, meta := range i.authorised {
		if meta.role == apiauth.RoleOwner {
			n++
		}
	}
	return n
}

// findLocked returns the enrolment behind a device id. The caller holds the
// mutex.
func (i *Identity) findLocked(id string) (string, *appMeta) {
	for key, meta := range i.authorised {
		if deviceID(key) == id {
			return key, meta
		}
	}
	return "", nil
}

// GrantFor is one enrolment as it stands right now.
//
// Read on every privileged request rather than at the handshake, because a
// socket outlives both a revoke and a role change. Three layers stop a revoked
// phone and none of them is enough alone: the session is torn down, the next
// handshake fails, and this — the one that closes the window where a socket
// outlives the revoke. It is also the only thing that makes a demotion bite
// on a session that is already open and working.
//
// The second result is false when the enrolment is gone entirely, which is a
// revoke. A grant that merely changed comes back with its new role, and the
// caller is expected to start using it rather than to end the session: a
// demoted owner is still a viewer, and telling them their access was withdrawn
// would be a sentence the code does not mean.
//
// The honest limit belongs beside it: revocation is immediate at the box.
// Nothing can un-send bytes already in a phone's cache.
func (i *Identity) GrantFor(id string) (Grant, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	key, meta := i.findLocked(id)
	if meta == nil {
		return Grant{}, false
	}
	if meta.role == apiauth.RoleWatch && i.authorised[meta.parent] == nil {
		// Revoke removes a phone's watches with it, so this is only a
		// guard: a watch never outlives the phone that vouches for it.
		return Grant{}, false
	}
	grant := i.grantLocked(key, meta)
	if meta.agent != nil && grant.Agent == nil {
		// The same guard for an agent and the phone that approved it.
		return Grant{}, false
	}
	return grant, true
}

// grantLocked is one enrolment as a Grant. The caller holds the mutex.
func (i *Identity) grantLocked(key string, meta *appMeta) Grant {
	grant := Grant{DeviceID: deviceID(key), Role: meta.role, Epoch: meta.epoch}
	if parent := i.authorised[meta.parent]; meta.parent != "" && parent != nil {
		grant.Parent = deviceID(meta.parent)
		grant.ParentRole = parent.role
	}
	if meta.agent != nil {
		if g, approverRole, ok := i.agentGrantLocked(meta); ok {
			grant.Agent = &g
			grant.ParentRole = approverRole
		}
	}
	return grant
}

// SetRole changes what one paired phone may do.
//
// It takes effect at once on a session that is already open, and the mechanism
// is GrantFor rather than anything here: both doors re-read the grant on every
// privileged request, so a phone demoted while it is connected loses its writes
// at the very next one rather than at its next reconnect. The epoch moves
// alongside, as the count of how many times this grant has changed.
//
// Refused for the only owner, here rather than in the API layer — otherwise
// the box's own web UI could do what the app cannot, and the protection would
// be a property of the screen instead of a property of the box.
//
// Refused at both doors, the box's own page included. Removing the last owner
// there is allowed — see Revoke — because a household whose phone is gone has
// to be able to empty the list and start again. Stepping the last owner down
// leaves that same box carrying a list of phones that can only look, which is
// the shape of the problem rather than a way out of it.
func (i *Identity) SetRole(id, role string) error {
	if err := phoneRole(role); err != nil {
		return err
	}

	i.mu.Lock()
	key, meta := i.findLocked(id)
	if meta == nil {
		i.mu.Unlock()
		return ErrUnknownDevice
	}
	if meta.role == apiauth.RoleWatch {
		// A watch is a watch until it is removed. Its reach follows its
		// phone's role, not a role of its own.
		i.mu.Unlock()
		return ErrWatchRole
	}
	if meta.agent != nil {
		// An agent's grant changes only by agent.widen, with an approval.
		i.mu.Unlock()
		return ErrAgentRow
	}
	if meta.role == role {
		// Nothing changed, so nothing is bumped. An epoch that moved for a
		// write that changed nothing would make every live session re-read a
		// grant that is the same as the one it holds.
		i.mu.Unlock()
		return nil
	}
	if meta.role == apiauth.RoleOwner && i.ownerCountLocked() == 1 {
		i.mu.Unlock()
		return ErrLastOwnerProtected
	}
	promoted := role == apiauth.RoleOwner
	if meta.role == apiauth.RoleOwner {
		i.cancelOwnerInvitesLocked(key)
	}
	meta.role = role
	meta.epoch++
	// The phone's watches hold what it holds, so their grants changed too.
	// Their epochs move with it, and the next roster carries the narrower
	// scopes.
	for _, watch := range i.authorised {
		if watch.parent == key {
			watch.epoch++
		}
	}
	i.mu.Unlock()

	err := i.save()
	i.changed()
	if promoted {
		// A phone made an owner can do anything, give access away included:
		// every owner hears of it, as of a new owner's pairing.
		i.ownerAdded()
	}
	return err
}

// Devices lists every paired phone, most recently seen first, each followed by
// its watches.
func (i *Identity) Devices() []DeviceInfo {
	i.mu.Lock()
	lastOwner := i.ownerCountLocked() == 1
	var phones []DeviceInfo
	watches := map[string][]DeviceInfo{}
	for key, meta := range i.authorised {
		if meta.agent != nil {
			// Agents have their own list. See Agents.
			continue
		}
		d := DeviceInfo{
			ID: deviceID(key), AddedAtMs: meta.addedAtMs, LastSeenMs: meta.lastSeenMs,
			Role:      meta.role,
			LastOwner: lastOwner && meta.role == apiauth.RoleOwner,
		}
		if meta.parent != "" {
			d.Parent = deviceID(meta.parent)
			watches[d.Parent] = append(watches[d.Parent], d)
			continue
		}
		phones = append(phones, d)
	}
	i.mu.Unlock()

	recentFirst := func(rows []DeviceInfo) {
		sort.Slice(rows, func(a, b int) bool {
			if rows[a].LastSeenMs != rows[b].LastSeenMs {
				return rows[a].LastSeenMs > rows[b].LastSeenMs
			}
			return rows[a].ID < rows[b].ID
		})
	}
	recentFirst(phones)
	out := make([]DeviceInfo, 0, len(phones))
	for _, phone := range phones {
		out = append(out, phone)
		recentFirst(watches[phone.ID])
		out = append(out, watches[phone.ID]...)
	}
	return out
}

// ErrUnknownDevice is a revoke aimed at an id no paired phone carries.
var ErrUnknownDevice = errors.New("appenroll: no such device")

// Presence says whether whoever is asking is standing at the box.
//
// It is not a role and it is not a scope: an owner's phone holds every scope
// this box grants and still cannot prove where it is. Presence is the one
// thing only the LAN door establishes, and here it settles exactly one
// question — whether the last owner may go.
type Presence bool

const (
	// AtTheBox is a request off the box's own page, on the home network.
	AtTheBox Presence = true
	// OverASession is an enrolled phone, which could be anywhere on earth.
	OverASession Presence = false
)

// Removed is one enrolment a revoke took away: its device id and its full key,
// so the caller can end the sessions the key is running and sweep what the
// device left behind.
type Removed struct {
	ID  string
	Key []byte
}

// Revoke forgets a device by its id and returns what it removed: the device
// first, then, for a phone, every watch it added. The caller tears down the
// sessions each key is running right now. The next handshake from any of them
// meets ErrNoPairing like any stranger's.
//
// Removal runs from phone to watch and never back. A watch holds what its phone
// vouches for, so it goes with the phone; removing a watch touches nothing
// else.
//
// Removing a guest and locking out a phone are the same action, deliberately.
// Sharing does not get its own gesture with its own bugs: a shared phone is a
// row in the same list, removed by the same button.
//
// Refused for the only owner over a session, and allowed at the box.
//
// What the refusal guards against is a phone locking a household out of its
// own box from anywhere in the world: remove the last owner remotely and
// nobody can administer the box, and nothing done remotely can undo that.
// Presence is the way back — whoever is standing at the box can mint a fresh
// owner's code on the same page the Remove button is on. So at the box the
// refusal protects nothing, and all it does is stop a household emptying the
// list after a phone is lost or starting clean.
//
// The decision stays here rather than in the API layer. What is new is that
// the box is told which door the request came through; which screen drew the
// button still decides nothing.
func (i *Identity) Revoke(id string, from Presence) ([]Removed, error) {
	i.mu.Lock()
	fullKey, meta := i.findLocked(id)
	if meta == nil {
		i.mu.Unlock()
		return nil, ErrUnknownDevice
	}
	if from == OverASession && meta.role == apiauth.RoleOwner && i.ownerCountLocked() == 1 {
		i.mu.Unlock()
		return nil, ErrLastOwnerProtected
	}
	i.cancelOwnerInvitesLocked(fullKey)
	gone := []string{fullKey}
	for key, other := range i.authorised {
		// A phone's watches go with it, and so do the agents it approved:
		// neither may hold more than the phone that vouches for it, and a
		// phone that is gone vouches for nothing.
		if other.parent == fullKey || (other.agent != nil && other.agent.ApprovedBy == fullKey) {
			gone = append(gone, key)
		}
	}
	sort.Strings(gone[1:])
	for _, key := range gone {
		delete(i.authorised, key)
	}
	i.mu.Unlock()

	err := i.save()
	i.changed()
	if err != nil {
		return nil, err
	}
	out := make([]Removed, 0, len(gone))
	for _, key := range gone {
		raw, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil {
			// The key was minted by EncodeToString; this cannot happen
			// outside a corrupted file, and the revoke itself already stuck.
			continue
		}
		out = append(out, Removed{ID: deviceID(key), Key: raw})
	}
	return out, nil
}

// RosterEntry is one paired device with its whole key, for the roster the box
// signs for FTW Cloud. The service learns devices from nothing else, and it
// needs the key to recognise the device's Noise handshake.
type RosterEntry struct {
	ID    string
	Key   []byte
	Role  string
	Epoch uint64
	// ParentRole is the role a watch's phone holds now, empty for a phone.
	// The scopes the roster carries for a watch are those both roles share.
	// For an agent row it is the role its approver holds now.
	ParentRole string
	// Agent is an agent row's grant, nil for a phone or a watch.
	Agent *AgentGrant
}

// Roster lists every paired device, watches included, sorted by id.
func (i *Identity) Roster() []RosterEntry {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.rosterLocked()
}

func (i *Identity) rosterLocked() []RosterEntry {
	out := make([]RosterEntry, 0, len(i.authorised))
	for key, meta := range i.authorised {
		raw, err := base64.RawURLEncoding.DecodeString(key)
		if err != nil {
			continue
		}
		grant := i.grantLocked(key, meta)
		if meta.role == apiauth.RoleWatch && grant.Parent == "" {
			continue
		}
		if meta.agent != nil && (grant.Agent == nil || grant.Agent.Expired) {
			// FTW Cloud stops admitting an agent at its expiry by its
			// own clock; a roster after that leaves it out as well.
			continue
		}
		out = append(out, RosterEntry{ID: grant.DeviceID, Key: raw, Role: meta.role, Epoch: meta.epoch,
			ParentRole: grant.ParentRole, Agent: grant.Agent})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// OnChange registers fn to run after a pairing, a role change or a revoke has
// been saved. A reconnect's lastSeen stamp is not a change.
func (i *Identity) OnChange(fn func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.onChange = append(i.onChange, fn)
}

func (i *Identity) changed() {
	i.mu.Lock()
	fns := append([]func(){}, i.onChange...)
	i.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// AuthorisedCount is for the API and for tests. The keys themselves stay here.
func (i *Identity) AuthorisedCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.authorised)
}
