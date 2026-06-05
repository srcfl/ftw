package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

// a valid 43-char base64url userHandle (32 bytes) for tests.
const testHandle = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO_-"

func newTestBlobStore(t *testing.T) *WalletBlobStore {
	t.Helper()
	s, err := NewWalletBlobStore(t.TempDir(), 4096, 8)
	if err != nil {
		t.Fatalf("NewWalletBlobStore: %v", err)
	}
	return s
}

func newWriteKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return pub, priv
}

// putSigned signs the canonical write message with priv and Puts. Reuse the same
// (pub, priv) across writes to one handle — the store TOFU-pins it.
func putSigned(s *WalletBlobStore, pub ed25519.PublicKey, priv ed25519.PrivateKey, handle string, ct, nonce []byte, version int) error {
	sig := ed25519.Sign(priv, blobWriteMessage(handle, version, nonce, ct))
	return s.Put(handle, ct, nonce, pub, sig, version)
}

func TestWalletBlob_PutGetRoundTrip(t *testing.T) {
	s := newTestBlobStore(t)
	pub, priv := newWriteKey(t)
	ct := []byte("opaque-ciphertext")
	nc := []byte("twelvebytenon")
	if err := putSigned(s, pub, priv, testHandle, ct, nc, 1); err != nil {
		t.Fatalf("Put: %v", err)
	}
	gotCT, gotNC, ver, ok := s.Get(testHandle)
	if !ok || ver != 1 || string(gotCT) != "opaque-ciphertext" || string(gotNC) != "twelvebytenon" {
		t.Fatalf("Get = %q,%q,%d,%v want round-trip v1", gotCT, gotNC, ver, ok)
	}
	if _, _, _, ok := s.Get("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"); ok {
		t.Fatal("unknown handle must report ok=false")
	}
	// The returned slice must be a copy — mutating it must not corrupt the store.
	gotCT[0] = 'X'
	again, _, _, _ := s.Get(testHandle)
	if string(again) != "opaque-ciphertext" {
		t.Fatal("Get must return a copy; stored ciphertext was mutated")
	}
}

// Writer authentication: a forged signature, a missing key, or a DIFFERENT key on
// an existing wallet are all rejected — only the holder of the TOFU-pinned write
// key (the owner's passkey-derived key) can write.
func TestWalletBlob_WriterAuth(t *testing.T) {
	s := newTestBlobStore(t)
	pub, priv := newWriteKey(t)
	ct, nc := []byte("ct"), []byte("nc")

	// Malformed key/sig lengths → unauthorized (never a panic).
	if err := s.Put(testHandle, ct, nc, []byte("short"), make([]byte, 64), 1); err != ErrUnauthorizedWrite {
		t.Fatalf("short pubkey: err=%v want ErrUnauthorizedWrite", err)
	}
	if err := s.Put(testHandle, ct, nc, pub, []byte("shortsig"), 1); err != ErrUnauthorizedWrite {
		t.Fatalf("short sig: err=%v want ErrUnauthorizedWrite", err)
	}
	// A signature over the WRONG message (tampered ciphertext) does not verify.
	badSig := ed25519.Sign(priv, blobWriteMessage(testHandle, 1, nc, []byte("other-ct")))
	if err := s.Put(testHandle, ct, nc, pub, badSig, 1); err != ErrUnauthorizedWrite {
		t.Fatalf("tampered sig: err=%v want ErrUnauthorizedWrite", err)
	}
	// A correct first write TOFU-pins the key.
	if err := putSigned(s, pub, priv, testHandle, ct, nc, 1); err != nil {
		t.Fatalf("authorized first write: %v", err)
	}
	// A DIFFERENT key — even with a valid self-signature — cannot take over.
	pub2, priv2 := newWriteKey(t)
	sig2 := ed25519.Sign(priv2, blobWriteMessage(testHandle, 2, nc, ct))
	if err := s.Put(testHandle, ct, nc, pub2, sig2, 2); err != ErrUnauthorizedWrite {
		t.Fatalf("takeover with new key: err=%v want ErrUnauthorizedWrite", err)
	}
	// The pinned key keeps working.
	if err := putSigned(s, pub, priv, testHandle, ct, nc, 2); err != nil {
		t.Fatalf("pinned key advance: %v", err)
	}
}

func TestWalletBlob_VersionBounds(t *testing.T) {
	s := newTestBlobStore(t)
	pub, priv := newWriteKey(t)
	// Non-positive versions are always rejected (before the wallet exists).
	for _, v := range []int{0, -1, -1000} {
		if err := putSigned(s, pub, priv, testHandle, []byte("x"), []byte("n"), v); err != ErrVersionConflict {
			t.Fatalf("Put version=%d: err=%v want ErrVersionConflict", v, err)
		}
	}
	// A first write that jumps past the bound (e.g. MaxInt to lock the wallet) is refused.
	if err := putSigned(s, pub, priv, testHandle, []byte("x"), []byte("n"), maxVersionJump+1); err != ErrVersionConflict {
		t.Fatalf("new-wallet over-jump: err=%v want ErrVersionConflict", err)
	}
	if err := putSigned(s, pub, priv, testHandle, []byte("x"), []byte("n"), 1); err != nil {
		t.Fatalf("first write v1: %v", err)
	}
	// An advance that jumps past the bound (lock-out) is refused even though it is
	// strictly greater than the stored version.
	if err := putSigned(s, pub, priv, testHandle, []byte("x"), []byte("n"), 1+maxVersionJump+1); err != ErrVersionConflict {
		t.Fatalf("advance over-jump: err=%v want ErrVersionConflict", err)
	}
	if err := putSigned(s, pub, priv, testHandle, []byte("x"), []byte("n"), 2); err != nil {
		t.Fatalf("advance v2: %v", err)
	}
}

func TestWalletBlob_VersionMonotonic(t *testing.T) {
	s := newTestBlobStore(t)
	pub, priv := newWriteKey(t)
	if err := putSigned(s, pub, priv, testHandle, []byte("v2"), []byte("n"), 2); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := putSigned(s, pub, priv, testHandle, []byte("dupe"), []byte("n"), 2); err != ErrVersionConflict {
		t.Fatalf("equal version: err=%v want ErrVersionConflict", err)
	}
	if err := putSigned(s, pub, priv, testHandle, []byte("old"), []byte("n"), 1); err != ErrVersionConflict {
		t.Fatalf("lower version: err=%v want ErrVersionConflict", err)
	}
	if err := putSigned(s, pub, priv, testHandle, []byte("v3"), []byte("n"), 3); err != nil {
		t.Fatalf("higher version: %v", err)
	}
	if _, _, ver, _ := s.Get(testHandle); ver != 3 {
		t.Fatalf("version = %d want 3", ver)
	}
}

func TestWalletBlob_SizeCap(t *testing.T) {
	s := newTestBlobStore(t) // maxBytes 4096
	pub, priv := newWriteKey(t)
	if err := putSigned(s, pub, priv, testHandle, make([]byte, 4097), []byte("n"), 1); err != ErrBlobTooLarge {
		t.Fatalf("oversize: err=%v want ErrBlobTooLarge", err)
	}
	if err := putSigned(s, pub, priv, testHandle, make([]byte, 4096), []byte("n"), 1); err != nil {
		t.Fatalf("exactly at cap must succeed: %v", err)
	}
}

func TestWalletBlob_WalletCap(t *testing.T) {
	s, err := NewWalletBlobStore(t.TempDir(), 4096, 2)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	pub, priv := newWriteKey(t)
	h := func(c byte) string { return strings.Repeat(string(c), 43) }
	if err := putSigned(s, pub, priv, h('a'), []byte("x"), []byte("n"), 1); err != nil {
		t.Fatalf("blob 1: %v", err)
	}
	if err := putSigned(s, pub, priv, h('b'), []byte("x"), []byte("n"), 1); err != nil {
		t.Fatalf("blob 2: %v", err)
	}
	// A THIRD distinct wallet exceeds the cap.
	if err := putSigned(s, pub, priv, h('c'), []byte("x"), []byte("n"), 1); err != ErrTooManyBlobs {
		t.Fatalf("over cap: err=%v want ErrTooManyBlobs", err)
	}
	// Updating an existing wallet is always allowed (not a new wallet).
	if err := putSigned(s, pub, priv, h('a'), []byte("y"), []byte("n"), 2); err != nil {
		t.Fatalf("update existing at cap: %v", err)
	}
}

func TestWalletBlob_RejectsBadHandle(t *testing.T) {
	s := newTestBlobStore(t)
	for _, bad := range []string{
		"short",
		"../etc/passwd",
		strings.Repeat("a", 42),
		strings.Repeat("a", 87),
		"has/slash" + strings.Repeat("a", 40),
		"has.dot" + strings.Repeat("a", 40),
	} {
		// Bad handle is rejected before any signature check.
		if err := s.Put(bad, []byte("x"), []byte("n"), make([]byte, 32), make([]byte, 64), 1); err != ErrBadUserHandle {
			t.Errorf("Put(%q): err=%v want ErrBadUserHandle", bad, err)
		}
		if _, _, _, ok := s.Get(bad); ok {
			t.Errorf("Get(%q) returned ok=true for an invalid handle", bad)
		}
	}
}

func TestWalletBlob_PersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewWalletBlobStore(dir, 4096, 8)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	pub, priv := newWriteKey(t)
	if err := putSigned(s1, pub, priv, testHandle, []byte("durable-ct"), []byte("durable-nc"), 7); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Reopen — the blob (and its pinned write key) must load from disk.
	s2, err := NewWalletBlobStore(dir, 4096, 8)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	ct, nc, ver, ok := s2.Get(testHandle)
	if !ok || ver != 7 || string(ct) != "durable-ct" || string(nc) != "durable-nc" {
		t.Fatalf("reloaded = %q,%q,%d,%v want durable v7", ct, nc, ver, ok)
	}
	// The pinned key still gates writes after reload: a stranger can't take over.
	pub2, priv2 := newWriteKey(t)
	sig2 := ed25519.Sign(priv2, blobWriteMessage(testHandle, 8, []byte("n"), []byte("x")))
	if err := s2.Put(testHandle, []byte("x"), []byte("n"), pub2, sig2, 8); err != ErrUnauthorizedWrite {
		t.Fatalf("takeover after reload: err=%v want ErrUnauthorizedWrite", err)
	}
	if err := putSigned(s2, pub, priv, testHandle, []byte("x"), []byte("n"), 8); err != nil {
		t.Fatalf("pinned key after reload: %v", err)
	}
}

func TestWalletBlob_GCEvictsIdle(t *testing.T) {
	s := newTestBlobStore(t)
	pub, priv := newWriteKey(t)
	if err := putSigned(s, pub, priv, testHandle, []byte("x"), []byte("n"), 1); err != nil {
		t.Fatalf("put: %v", err)
	}
	if n := s.GC(time.Hour); n != 0 {
		t.Fatalf("fresh blob must survive GC(1h), evicted %d", n)
	}
	if n := s.GC(0); n != 0 {
		t.Fatalf("GC(0) must evict nothing, evicted %d", n)
	}
	if n := s.GC(-time.Nanosecond); n != 0 {
		t.Fatalf("GC(<=0) must evict nothing, evicted %d", n)
	}
	s.mu.Lock()
	s.blobs[testHandle].touchedAt = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	if n := s.GC(time.Minute); n != 1 {
		t.Fatalf("stale blob: evicted %d want 1", n)
	}
	if _, _, _, ok := s.Get(testHandle); ok {
		t.Fatal("evicted blob still present")
	}
}
