package main

import (
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

func TestWalletBlob_PutGetRoundTrip(t *testing.T) {
	s := newTestBlobStore(t)
	ct := []byte("opaque-ciphertext")
	nc := []byte("twelvebytenon")
	if err := s.Put(testHandle, ct, nc, 1); err != nil {
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

func TestWalletBlob_VersionMonotonic(t *testing.T) {
	s := newTestBlobStore(t)
	if err := s.Put(testHandle, []byte("v2"), []byte("n"), 2); err != nil {
		t.Fatalf("first put: %v", err)
	}
	// Same version → conflict (lost-update guard).
	if err := s.Put(testHandle, []byte("dupe"), []byte("n"), 2); err != ErrVersionConflict {
		t.Fatalf("equal version: err=%v want ErrVersionConflict", err)
	}
	// Lower version (rollback) → conflict.
	if err := s.Put(testHandle, []byte("old"), []byte("n"), 1); err != ErrVersionConflict {
		t.Fatalf("lower version: err=%v want ErrVersionConflict", err)
	}
	// Strictly greater → accepted.
	if err := s.Put(testHandle, []byte("v3"), []byte("n"), 3); err != nil {
		t.Fatalf("higher version: %v", err)
	}
	if _, _, ver, _ := s.Get(testHandle); ver != 3 {
		t.Fatalf("version = %d want 3", ver)
	}
}

func TestWalletBlob_VersionBounds(t *testing.T) {
	s := newTestBlobStore(t)
	// Non-positive versions are always rejected.
	for _, v := range []int{0, -1, -1000} {
		if err := s.Put(testHandle, []byte("x"), []byte("n"), v); err != ErrVersionConflict {
			t.Fatalf("Put version=%d: err=%v want ErrVersionConflict", v, err)
		}
	}
	// A first write that jumps past the bound (e.g. an attacker setting MaxInt to
	// lock the wallet) is refused.
	if err := s.Put(testHandle, []byte("x"), []byte("n"), maxVersionJump+1); err != ErrVersionConflict {
		t.Fatalf("new-wallet over-jump: err=%v want ErrVersionConflict", err)
	}
	// A legitimate first write at 1 is fine.
	if err := s.Put(testHandle, []byte("x"), []byte("n"), 1); err != nil {
		t.Fatalf("first write v1: %v", err)
	}
	// An advance that jumps past the bound (MaxInt lock-out) is refused even though
	// it is strictly greater than the stored version.
	if err := s.Put(testHandle, []byte("x"), []byte("n"), 1+maxVersionJump+1); err != ErrVersionConflict {
		t.Fatalf("advance over-jump (lock-out attempt): err=%v want ErrVersionConflict", err)
	}
	// A normal +1 advance still works.
	if err := s.Put(testHandle, []byte("x"), []byte("n"), 2); err != nil {
		t.Fatalf("advance v2: %v", err)
	}
}

func TestWalletBlob_SizeCap(t *testing.T) {
	s := newTestBlobStore(t) // maxBytes 4096
	if err := s.Put(testHandle, make([]byte, 4097), []byte("n"), 1); err != ErrBlobTooLarge {
		t.Fatalf("oversize: err=%v want ErrBlobTooLarge", err)
	}
	if err := s.Put(testHandle, make([]byte, 4096), []byte("n"), 1); err != nil {
		t.Fatalf("exactly at cap must succeed: %v", err)
	}
}

func TestWalletBlob_WalletCap(t *testing.T) {
	s, err := NewWalletBlobStore(t.TempDir(), 4096, 2)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	h := func(c byte) string { return strings.Repeat(string(c), 43) }
	if err := s.Put(h('a'), []byte("x"), []byte("n"), 1); err != nil {
		t.Fatalf("blob 1: %v", err)
	}
	if err := s.Put(h('b'), []byte("x"), []byte("n"), 1); err != nil {
		t.Fatalf("blob 2: %v", err)
	}
	// A THIRD distinct wallet exceeds the cap.
	if err := s.Put(h('c'), []byte("x"), []byte("n"), 1); err != ErrTooManyBlobs {
		t.Fatalf("over cap: err=%v want ErrTooManyBlobs", err)
	}
	// But UPDATING an existing wallet is always allowed (not a new wallet).
	if err := s.Put(h('a'), []byte("y"), []byte("n"), 2); err != nil {
		t.Fatalf("update existing at cap: %v", err)
	}
}

func TestWalletBlob_RejectsBadHandle(t *testing.T) {
	s := newTestBlobStore(t)
	for _, bad := range []string{
		"short",
		"../etc/passwd",
		strings.Repeat("a", 42),  // one under min
		strings.Repeat("a", 87),  // one over max
		"has/slash" + strings.Repeat("a", 40),
		"has.dot" + strings.Repeat("a", 40),
	} {
		if err := s.Put(bad, []byte("x"), []byte("n"), 1); err != ErrBadUserHandle {
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
	if err := s1.Put(testHandle, []byte("durable-ct"), []byte("durable-nc"), 7); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Reopen — the blob must load from disk.
	s2, err := NewWalletBlobStore(dir, 4096, 8)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	ct, nc, ver, ok := s2.Get(testHandle)
	if !ok || ver != 7 || string(ct) != "durable-ct" || string(nc) != "durable-nc" {
		t.Fatalf("reloaded = %q,%q,%d,%v want durable v7", ct, nc, ver, ok)
	}
}

func TestWalletBlob_GCEvictsIdle(t *testing.T) {
	s := newTestBlobStore(t)
	if err := s.Put(testHandle, []byte("x"), []byte("n"), 1); err != nil {
		t.Fatalf("put: %v", err)
	}
	if n := s.GC(time.Hour); n != 0 {
		t.Fatalf("fresh blob must survive GC(1h), evicted %d", n)
	}
	// idle 0 evicts nothing by contract.
	if n := s.GC(0); n != 0 {
		t.Fatalf("GC(0) must evict nothing, evicted %d", n)
	}
	// A negative-age sweep (everything older than "now") evicts it.
	if n := s.GC(-time.Nanosecond); n != 0 {
		t.Fatalf("GC(<=0) must evict nothing, evicted %d", n)
	}
	// Force staleness: backdate touchedAt, then a tiny idle window evicts.
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
