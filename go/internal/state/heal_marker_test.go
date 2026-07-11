package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The verified-good marker is PERSISTENT: writing it makes markerPresent true,
// and it stays true across repeated checks (it is not consumed on read). Only an
// explicit remove clears it.
func TestMarkerPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	if markerPresent(path) {
		t.Fatal("markerPresent reported a marker before one was written")
	}
	writeCleanMarker(path)
	if !markerPresent(path) {
		t.Fatalf("writeCleanMarker did not create %s", cleanMarkerPath(path))
	}
	// Reading does not consume it — still present.
	if !markerPresent(path) {
		t.Fatal("marker was consumed on read — it must persist")
	}
	if err := os.Remove(cleanMarkerPath(path)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	if markerPresent(path) {
		t.Fatal("marker still present after removal")
	}
}

// A present marker makes openChecked SKIP the integrity check on state.db: we
// point it at a corrupt state.db, drop a marker, and assert it is returned
// without a heal event — and that the marker PERSISTS (not consumed), so the
// next boot is fast too.
func TestOpenCheckedSkipsWithMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	writePopulated(t, path, 200)
	corruptAt(t, path, 8192)
	writeCleanMarker(path)

	db, ev, err := openChecked(path, tierState, 1000)
	if err != nil {
		t.Fatalf("openChecked with marker: %v", err)
	}
	defer db.Close()
	if ev != nil {
		t.Fatalf("marker should skip the check, but a heal ran: %+v", ev)
	}
	if !markerPresent(path) {
		t.Error("marker must persist across the skip (not be consumed)")
	}
}

// The skip is scoped to state.db. cache.db is tiny + disposable, so it must
// ALWAYS be checked — a marker beside it is ignored and corruption still rebuilds.
func TestOpenCheckedCacheIgnoresMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.db")
	writePopulated(t, path, 200)
	corruptAt(t, path, 8192)
	writeCleanMarker(path) // must be ignored for cache

	db, ev, err := openChecked(path, tierCache, 1717430000000)
	if err != nil {
		t.Fatalf("openChecked cache: %v", err)
	}
	defer db.Close()
	if ev == nil || ev.Action != healRebuilt || ev.Tier != tierCache {
		t.Fatalf("cache must be checked despite a marker — want rebuilt/cache, got %+v", ev)
	}
}

// Open arms the marker for state.db (only), it persists across a Close, and a
// re-open does NOT consume it — so restarts stay fast without depending on how
// the process exited.
func TestOpenArmsMarkerAndItPersists(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")

	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// Armed for state.db, not for cache.db.
	if !markerPresent(statePath) {
		t.Error("Open did not arm the state.db marker")
	}
	if markerPresent(filepath.Join(dir, "cache.db")) {
		t.Error("Open armed a marker for cache.db, but cache must always be checked")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Persists across Close (Close must not touch it).
	if !markerPresent(statePath) {
		t.Error("marker did not survive Close — restarts would be slow")
	}
	// Re-open: still present afterwards (not consumed).
	st2, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if !markerPresent(statePath) {
		t.Error("re-open consumed the marker — it must persist")
	}
}

// verifyOnce on a corrupt DB must flag the store and remove the marker so the
// next boot runs the full check + heals. We build a Store directly over a
// corrupt file (bypassing Open's healing) to exercise the corrupt path.
func TestVerifyOnceArmsHealOnCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	writePopulated(t, path, 200)
	corruptAt(t, path, 8192)

	db, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{db: db, mainDBPath: path}
	writeCleanMarker(path) // armed from a past good boot

	s.verifyOnce(context.Background())

	s.healMu.Lock()
	corrupt := s.corrupt
	s.healMu.Unlock()
	if !corrupt {
		t.Error("verifyOnce did not flag a corrupt DB")
	}
	if markerPresent(path) {
		t.Error("verifyOnce did not remove the marker on detected corruption")
	}
}

func TestVerifyOnceCleanDBStaysHealthy(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	st.verifyOnce(context.Background())

	st.healMu.Lock()
	corrupt := st.corrupt
	st.healMu.Unlock()
	if corrupt {
		t.Error("verifyOnce flagged a healthy DB as corrupt")
	}
	if !markerPresent(statePath) {
		t.Error("verifyOnce on a clean DB must leave the marker armed")
	}
}

// A cancelled context means the scan was interrupted on shutdown, NOT that the
// DB is corrupt — verifyOnce must leave the marker in place.
func TestVerifyOnceCanceledIsNotCorruption(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Open already armed the marker; ensure it's there.
	if !markerPresent(statePath) {
		t.Fatal("precondition: Open should have armed the marker")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled → quick_check errors with a context error

	st.verifyOnce(ctx)

	st.healMu.Lock()
	corrupt := st.corrupt
	st.healMu.Unlock()
	if corrupt {
		t.Error("a cancelled (aborted) scan must NOT be treated as corruption")
	}
	if !markerPresent(statePath) {
		t.Error("a cancelled scan must leave the marker in place")
	}
}

// Close must cancel a running background scan and return promptly (not block on
// the scan), and must NOT remove the marker Open armed. This is the deploy path:
// a restart inside the scan window stays fast because the marker persists.
func TestCloseCancelsBackgroundVerifyAndKeepsMarker(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")
	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	st.VerifyInBackground()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !markerPresent(statePath) {
		t.Error("Close removed the marker — restarts in the scan window would be slow")
	}
}
