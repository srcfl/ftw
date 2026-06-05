package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanMarkerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// No marker yet.
	if consumeCleanMarker(path) {
		t.Fatal("consumeCleanMarker reported a marker before one was written")
	}
	// Write → present → consumed (single-use).
	writeCleanMarker(path)
	if _, err := os.Stat(cleanMarkerPath(path)); err != nil {
		t.Fatalf("writeCleanMarker did not create %s: %v", cleanMarkerPath(path), err)
	}
	if !consumeCleanMarker(path) {
		t.Fatal("consumeCleanMarker did not see the written marker")
	}
	if consumeCleanMarker(path) {
		t.Fatal("marker was not consumed — second consume still saw it")
	}
}

// A clean-shutdown marker must make openChecked SKIP the integrity check on the
// precious state DB: we point it at a corrupt state.db, drop a marker, and assert
// it is returned without a heal event (proving the check was skipped — without
// the marker this same DB heals, see TestOpenCheckedStateFreshWhenNoSnapshot).
func TestOpenCheckedSkipsCheckWithCleanMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	writePopulated(t, path, 200)
	corruptAt(t, path, 8192)
	writeCleanMarker(path)

	db, ev, err := openChecked(path, tierState, 1000)
	if err != nil {
		t.Fatalf("openChecked with clean marker: %v", err)
	}
	defer db.Close()
	if ev != nil {
		t.Fatalf("clean marker should skip the check, but a heal ran: %+v", ev)
	}
	// The marker is single-use: it must be gone after the open.
	if consumeCleanMarker(path) {
		t.Error("clean marker was not consumed on open")
	}
}

// The clean-shutdown fast path is scoped to state.db. cache.db is tiny +
// disposable, so it must ALWAYS be checked — a marker beside it is ignored and
// corruption still rebuilds.
func TestOpenCheckedCacheIgnoresCleanMarker(t *testing.T) {
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

func TestStoreCloseWritesCleanMarkerAndNextOpenSkips(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")

	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// state.db got a clean marker; cache.db must NOT (it is always checked).
	if _, err := os.Stat(cleanMarkerPath(statePath)); err != nil {
		t.Errorf("Close did not write clean marker for state.db: %v", err)
	}
	if _, err := os.Stat(cleanMarkerPath(filepath.Join(dir, "cache.db"))); err == nil {
		t.Error("Close wrote a clean marker for cache.db, but cache must always be checked")
	}

	// Re-open consumes the state.db marker (so a later crash forces a real check).
	st2, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := os.Stat(cleanMarkerPath(statePath)); err == nil {
		t.Error("Open did not consume the state.db clean marker")
	}
}

func TestStoreCloseSkipsMarkerWhenCorrupt(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")

	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a background verify having flagged corruption.
	st.healMu.Lock()
	st.corrupt = true
	st.healMu.Unlock()

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(cleanMarkerPath(statePath)); err == nil {
		t.Error("a corrupt store must NOT leave a clean marker (next boot must check + heal)")
	}
}

// verifyOnce on a corrupt DB must flag the store and remove any clean marker so
// the next boot runs the full check + heals. We build a Store directly over a
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
	writeCleanMarker(path) // pretend the previous shutdown was "clean"

	s.verifyOnce()

	s.healMu.Lock()
	corrupt := s.corrupt
	s.healMu.Unlock()
	if !corrupt {
		t.Error("verifyOnce did not flag a corrupt DB")
	}
	if consumeCleanMarker(path) {
		t.Error("verifyOnce did not remove the clean marker on detected corruption")
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

	st.verifyOnce()

	st.healMu.Lock()
	corrupt := st.corrupt
	st.healMu.Unlock()
	if corrupt {
		t.Error("verifyOnce flagged a healthy DB as corrupt")
	}
}
