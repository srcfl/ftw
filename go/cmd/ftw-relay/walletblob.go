package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// WalletBlobStore is the relay's BLIND per-wallet directory store for the
// multi-tenant home route. It holds, keyed by an opaque WebAuthn userHandle, a
// ciphertext blob the BROWSER produced (the list of a wallet's instances,
// AES-GCM-encrypted under a key derived from the passkey via WebAuthn PRF). The
// relay NEVER decrypts or parses the ciphertext — it is an opaque byte string in
// and out. This lets a fresh device with a synced passkey fetch + decrypt its
// home list before any P2P channel exists, while the relay learns nothing about
// which Pi belongs to whom (the site_ids live encrypted inside the blob).
//
// It is the ONE piece of durable relay state: blobs persist to one JSON file per
// userHandle under dir, so they survive a relay restart. Everything else in the
// relay stays in-memory and ephemeral.
//
// Invariants enforced here:
//   - opaque: ciphertext/nonce are []byte, never inspected;
//   - bounded: each blob is capped at maxBytes and the number of distinct
//     wallets at maxBlobs, so the unauthenticated PUT can't exhaust disk/memory;
//   - monotonic: a Put whose version is not strictly greater than the stored one
//     is refused (ErrVersionConflict) — a lost-update / rollback guard for two of
//     a user's devices editing concurrently;
//   - safe keys: userHandle is validated (base64url, bounded length) before it is
//     ever used as a map key or a filename, so it can't traverse the directory.
type WalletBlobStore struct {
	mu       sync.Mutex
	blobs    map[string]*blobEntry
	dir      string
	maxBytes int
	maxBlobs int
}

type blobEntry struct {
	ciphertext  []byte
	nonce       []byte
	writePub    []byte // Ed25519 public key (32 B) TOFU-pinned for this wallet's writes
	version     int
	updatedAtMs int64
	touchedAt   time.Time
}

// blobFile is the on-disk JSON shape (base64 so it round-trips as text).
type blobFile struct {
	CiphertextB64 string `json:"ciphertext_b64"`
	NonceB64      string `json:"nonce_b64"`
	WritePubB64   string `json:"write_pub_b64"`
	Version       int    `json:"version"`
	UpdatedAtMs   int64  `json:"updated_at_ms"`
}

const (
	// defaultMaxWalletBlobs bounds the number of distinct wallets the store holds,
	// so a flood of (validly self-signed) PUTs for random userHandles can't grow
	// disk/memory without limit. Wallet blobs are NOT time-GC'd (that would drop a
	// TOFU-pinned write key — see the janitor note), so this count cap is the sole
	// bound; it is generous because each entry is small and a real tenant
	// population is well below it. The residual is an enrollment-capacity DoS
	// (filling the cap blocks NEW wallets, never overwrites existing ones).
	defaultMaxWalletBlobs = 65536
	// blobFileSuffix is appended to the (validated, FS-safe) userHandle.
	blobFileSuffix = ".blob"
	// maxVersionJump bounds how far a single Put may advance the version. The raw
	// "strictly greater" guard would let a caller who learns a userHandle set
	// version to math.MaxInt and permanently lock the wallet out (no future write
	// can exceed it). Bounding the jump keeps version monotonic while making that
	// lock-out impossible; a legitimate client only ever increments by 1. (This
	// does NOT authenticate the writer — see walletBlobPut: a per-wallet write
	// signature is REQUIRED before the multi-tenant route goes live.)
	maxVersionJump = 1 << 20
)

var (
	// ErrVersionConflict is returned when a Put's version is not strictly greater
	// than the stored version (lost-update / rollback guard).
	ErrVersionConflict = errors.New("wallet blob version conflict")
	// ErrBlobTooLarge is returned when a Put's ciphertext exceeds maxBytes.
	ErrBlobTooLarge = errors.New("wallet blob too large")
	// ErrTooManyBlobs is returned when a Put for a NEW wallet would exceed maxBlobs.
	ErrTooManyBlobs = errors.New("too many wallet blobs")
	// ErrBadUserHandle is returned when the userHandle fails validation.
	ErrBadUserHandle = errors.New("invalid user handle")
	// ErrUnauthorizedWrite is returned when a Put's Ed25519 signature does not
	// verify against the wallet's TOFU-pinned write key (or the supplied key
	// differs from the pinned one). This is what stops a userHandle-knower who
	// lacks the owner's passkey-derived write key from overwriting a blob.
	ErrUnauthorizedWrite = errors.New("wallet blob write not authorized")
)

// blobWriteMessage is the canonical byte string a wallet's Ed25519 write key
// signs for a PUT. Binding handle + version + nonce + a hash of the ciphertext
// stops replay onto a different wallet/version and stops tampering with the
// stored bytes. The web client (instance-sync.js) MUST construct this identically:
//
//	"ftw-blob:v1:" + handle + ":" + version + ":" + base64url(nonce) + ":" + hex(sha256(ciphertext))
func blobWriteMessage(userHandle string, version int, nonce, ciphertext []byte) []byte {
	sum := sha256.Sum256(ciphertext)
	return []byte("ftw-blob:v1:" + userHandle + ":" +
		strconv.Itoa(version) + ":" +
		base64.RawURLEncoding.EncodeToString(nonce) + ":" +
		hex.EncodeToString(sum[:]))
}

// validUserHandle reports whether s is a safe opaque WebAuthn userHandle: a
// base64url token (RFC 4648 §5 alphabet, no padding) of bounded length. This is
// the SINGLE gate that keeps the handle safe to use as both a map key and a
// filename — it admits only [A-Za-z0-9_-], so it can never contain a path
// separator, "..", or NUL. Length 43..86 covers a 32..64-byte handle.
func validUserHandle(s string) bool {
	if len(s) < 43 || len(s) > 86 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// NewWalletBlobStore opens (creating if absent) the on-disk store at dir and
// loads any existing *.blob files into memory. maxBytes caps a single blob's
// ciphertext; maxBlobs caps the number of distinct wallets (<=0 → default).
func NewWalletBlobStore(dir string, maxBytes, maxBlobs int) (*WalletBlobStore, error) {
	if dir == "" {
		return nil, errors.New("wallet blob dir must be set")
	}
	if maxBlobs <= 0 {
		maxBlobs = defaultMaxWalletBlobs
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &WalletBlobStore{
		blobs:    make(map[string]*blobEntry),
		dir:      dir,
		maxBytes: maxBytes,
		maxBlobs: maxBlobs,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		// Stop at the wallet cap so a stuffed blob dir can't grow memory past the
		// runtime bound (a PUT enforces the same cap).
		if len(s.blobs) >= s.maxBlobs {
			break
		}
		name := e.Name()
		if filepath.Ext(name) != blobFileSuffix {
			continue
		}
		handle := name[:len(name)-len(blobFileSuffix)]
		if !validUserHandle(handle) {
			continue // ignore stray/garbage files; never trust an unvalidated name
		}
		// Only load REGULAR files of a sane size. e.Info() is lstat-based, so a
		// symlink is reported as a symlink (not followed) and skipped — a planted
		// symlink can neither redirect the read nor smuggle in an oversize blob.
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue
		}
		if s.maxBytes > 0 && info.Size() > int64(2*s.maxBytes)+1024 {
			continue // encoded JSON is ~4/3 the ciphertext; this bounds it generously
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		var bf blobFile
		if json.Unmarshal(raw, &bf) != nil {
			continue
		}
		ct, e1 := base64.StdEncoding.DecodeString(bf.CiphertextB64)
		nc, e2 := base64.StdEncoding.DecodeString(bf.NonceB64)
		wp, e3 := base64.StdEncoding.DecodeString(bf.WritePubB64)
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		if (s.maxBytes > 0 && len(ct) > s.maxBytes) || bf.Version <= 0 || len(wp) != ed25519.PublicKeySize {
			continue // enforce the runtime invariants on reload too (incl. a pinned write key)
		}
		s.blobs[handle] = &blobEntry{
			ciphertext:  ct,
			nonce:       nc,
			writePub:    wp,
			version:     bf.Version,
			updatedAtMs: bf.UpdatedAtMs,
			touchedAt:   time.Now(),
		}
	}
	return s, nil
}

// Get returns the stored ciphertext + nonce + version for a wallet, or ok=false
// when there is none (or the handle is invalid). Reading touches the entry so GC
// keeps live wallets warm.
func (s *WalletBlobStore) Get(userHandle string) (ciphertext, nonce []byte, version int, ok bool) {
	if !validUserHandle(userHandle) {
		return nil, nil, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, present := s.blobs[userHandle]
	if !present {
		return nil, nil, 0, false
	}
	e.touchedAt = time.Now()
	// Return copies so a caller can't mutate the stored bytes.
	ct := append([]byte(nil), e.ciphertext...)
	nc := append([]byte(nil), e.nonce...)
	return ct, nc, e.version, true
}

// Put stores a wallet's ciphertext blob. It enforces the size cap
// (ErrBlobTooLarge), the wallet-count cap for a brand-new wallet
// (ErrTooManyBlobs), strict version monotonicity (ErrVersionConflict), and
// userHandle validity (ErrBadUserHandle). On success it persists the blob to
// disk atomically (temp file + rename) so it survives a relay restart.
func (s *WalletBlobStore) Put(userHandle string, ciphertext, nonce, writePub, sig []byte, version int) error {
	if !validUserHandle(userHandle) {
		return ErrBadUserHandle
	}
	if version <= 0 {
		return ErrVersionConflict
	}
	if s.maxBytes > 0 && len(ciphertext) > s.maxBytes {
		return ErrBlobTooLarge
	}
	if len(writePub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return ErrUnauthorizedWrite
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, present := s.blobs[userHandle]
	if present {
		// The write key is TOFU-pinned at the first write and cannot change — a
		// caller presenting a different key (even a validly self-signed one) can
		// never take over an existing wallet's blob.
		if subtle.ConstantTimeCompare(prev.writePub, writePub) != 1 {
			return ErrUnauthorizedWrite
		}
	} else if len(s.blobs) >= s.maxBlobs {
		return ErrTooManyBlobs
	}
	// Authenticate the writer: the Ed25519 signature must verify against the
	// (pinned, or first-seen TOFU) write key over the canonical message. Without
	// the owner's passkey-derived write key this cannot be produced, so a mere
	// userHandle-knower cannot overwrite the blob.
	if !ed25519.Verify(writePub, blobWriteMessage(userHandle, version, nonce, ciphertext), sig) {
		return ErrUnauthorizedWrite
	}
	// Version monotonicity (bounded) — checked AFTER authentication so an
	// unauthenticated caller learns nothing about the stored version.
	if present {
		if version <= prev.version || version > prev.version+maxVersionJump {
			return ErrVersionConflict
		}
	} else if version > maxVersionJump {
		return ErrVersionConflict
	}
	now := time.Now()
	e := &blobEntry{
		ciphertext:  append([]byte(nil), ciphertext...),
		nonce:       append([]byte(nil), nonce...),
		writePub:    append([]byte(nil), writePub...),
		version:     version,
		updatedAtMs: now.UnixMilli(),
		touchedAt:   now,
	}
	if err := s.persist(userHandle, e); err != nil {
		return err
	}
	s.blobs[userHandle] = e
	return nil
}

// persist writes one blob to <dir>/<userHandle>.blob atomically. Caller holds mu.
func (s *WalletBlobStore) persist(userHandle string, e *blobEntry) error {
	bf := blobFile{
		CiphertextB64: base64.StdEncoding.EncodeToString(e.ciphertext),
		NonceB64:      base64.StdEncoding.EncodeToString(e.nonce),
		WritePubB64:   base64.StdEncoding.EncodeToString(e.writePub),
		Version:       e.version,
		UpdatedAtMs:   e.updatedAtMs,
	}
	raw, err := json.Marshal(bf)
	if err != nil {
		return err
	}
	final := filepath.Join(s.dir, userHandle+blobFileSuffix)
	tmp := final + ".tmp"
	// Drop any stale/planted tmp first, then create with O_EXCL so we can NEVER
	// follow a symlink an attacker left at the tmp name (the write would otherwise
	// be redirected). O_EXCL fails if anything exists at the path; the prior
	// Remove clears a leftover from a crashed write (removing a symlink unlinks the
	// link itself, not its target).
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, final)
}

// GC evicts wallets untouched for longer than idle (and removes their files).
// Returns how many were evicted. idle<=0 evicts nothing.
//
// WARNING: evicting a wallet drops its TOFU-pinned write key, so a later attacker
// PUT for the same userHandle would re-pin THEIR key and lock the owner out. This
// must therefore NOT be run on a schedule for the multi-tenant route (the janitor
// intentionally does not call it). It exists for tests and for an operator that
// knows a handle is truly abandoned. Proper abandoned-blob reclamation keeps a
// pin tombstone — a cutover concern.
func (s *WalletBlobStore) GC(idle time.Duration) int {
	if idle <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	removed := 0
	for handle, e := range s.blobs {
		if now.Sub(e.touchedAt) > idle {
			delete(s.blobs, handle)
			_ = os.Remove(filepath.Join(s.dir, handle+blobFileSuffix))
			removed++
		}
	}
	return removed
}

// Len reports the number of wallets held (observability/tests).
func (s *WalletBlobStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.blobs)
}
