package state

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotStateProducesValidCopy(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Put something precious in state.db (config table exists post-migrate).
	if _, err := st.db.Exec(`INSERT INTO config(key, value) VALUES ('k','v')`); err != nil {
		t.Fatal(err)
	}

	if err := st.SnapshotState(); err != nil {
		t.Fatalf("SnapshotState: %v", err)
	}
	snap := filepath.Join(dir, "state.db.snapshot")
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("snapshot not written: %v", err)
	}
	db, err := openRaw(snap)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ok, err := quickCheck(db)
	if err != nil || !ok {
		t.Errorf("snapshot not healthy: ok=%v err=%v", ok, err)
	}
	var v string
	if err := db.QueryRow(`SELECT value FROM config WHERE key='k'`).Scan(&v); err != nil || v != "v" {
		t.Errorf("snapshot missing precious row: %q err=%v", v, err)
	}
}

func TestSnapshotStateOverwritesPrevious(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SnapshotState(); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	// A second snapshot must succeed even though the destination exists.
	if err := st.SnapshotState(); err != nil {
		t.Fatalf("second snapshot should overwrite, got: %v", err)
	}
}

// TestStateRecoversViaSnapshotStateAndOpen exercises the full loop: snapshot
// written by SnapshotState is the exact file openChecked restores from after
// state.db corrupts. A path mismatch between the two would fail here.
func TestStateRecoversViaSnapshotStateAndOpen(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")

	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO config(key, value) VALUES ('precious','keep-me')`); err != nil {
		t.Fatal(err)
	}
	// Grow the file so an offset-8192 corruption lands on a real page.
	for i := 1; i <= 500; i++ {
		if _, err := st.db.Exec(`INSERT OR REPLACE INTO events(ts_ms, event) VALUES (?, 'x')`, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SnapshotState(); err != nil {
		t.Fatalf("SnapshotState: %v", err)
	}
	st.Close()

	corruptAt(t, statePath, 8192) // damage a page in the live (non-snapshot) file
	// Remove the verified-good marker so openChecked actually runs the boot check
	// and restores from snapshot — this is the state a background verify leaves
	// behind when it finds corruption (it removes the marker; see
	// TestMarkerSkipThenBackgroundVerifyHealsNextBoot for the full cycle).
	if err := os.Remove(cleanMarkerPath(statePath)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	st2, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()

	evs := st2.HealEvents()
	if len(evs) == 0 || evs[0].Tier != tierState || evs[0].Action != healRestored {
		t.Fatalf("want a state/restored heal event, got %+v", evs)
	}
	var v string
	if err := st2.db.QueryRow(`SELECT value FROM config WHERE key='precious'`).Scan(&v); err != nil || v != "keep-me" {
		t.Errorf("precious row not restored: %q err=%v", v, err)
	}
}

// TestMarkerSkipThenBackgroundVerifyHealsNextBoot documents the full cycle:
// with a verified-good marker present, openChecked SKIPS the (multi-minute on a
// large DB) boot check so startup stays fast even if the DB has rotted since.
// The background verify catches the rot and removes the marker, so the boot
// AFTER that runs the full check and restores from snapshot.
func TestMarkerSkipThenBackgroundVerifyHealsNextBoot(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.db")

	st, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO config(key, value) VALUES ('precious','keep-me')`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 500; i++ {
		if _, err := st.db.Exec(`INSERT OR REPLACE INTO events(ts_ms, event) VALUES (?, 'x')`, i); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SnapshotState(); err != nil {
		t.Fatalf("SnapshotState: %v", err)
	}
	st.Close() // Open armed the marker; it persists across Close

	corruptAt(t, statePath, 8192) // SD-rot after the DB was last verified good

	// Boot 1: marker present → boot check skipped → no heal yet (Open re-arms it).
	st1, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if evs := st1.HealEvents(); len(evs) != 0 {
		t.Fatalf("marker boot should skip the check, got heal events: %+v", evs)
	}
	// Background verify detects the rot and removes the marker.
	st1.verifyOnce(context.Background())
	if markerPresent(statePath) {
		t.Fatal("background verify should have removed the marker on corruption")
	}
	st1.Close()

	// Boot 2: no marker → full check → restore from snapshot.
	st2, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	evs := st2.HealEvents()
	if len(evs) == 0 || evs[0].Action != healRestored {
		t.Fatalf("boot after background-detected corruption should restore, got %+v", evs)
	}
	var v string
	if err := st2.db.QueryRow(`SELECT value FROM config WHERE key='precious'`).Scan(&v); err != nil || v != "keep-me" {
		t.Errorf("precious row not restored: %q err=%v", v, err)
	}
}
