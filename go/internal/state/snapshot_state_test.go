package state

import (
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
	// Model an UNCLEAN shutdown: with no clean-shutdown marker, openChecked runs
	// the boot integrity check and restores from snapshot. (After a CLEAN
	// shutdown the marker makes the boot skip the check — corruption is then
	// caught by the background verify and healed on the boot after; see
	// TestCleanShutdownDefersCorruptionToNextBoot.)
	if err := os.Remove(cleanMarkerPath(statePath)); err != nil {
		t.Fatalf("remove clean marker: %v", err)
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

// TestCleanShutdownDefersCorruptionToNextBoot documents the clean-shutdown fast
// path: corruption that appears AFTER a clean close is intentionally NOT caught
// on the next boot — the marker makes openChecked skip the (multi-minute on a
// large DB) integrity check so startup stays fast. The background verify catches
// the rot and arms a heal, so the boot AFTER that restores from snapshot.
func TestCleanShutdownDefersCorruptionToNextBoot(t *testing.T) {
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
	st.Close() // clean close → writes clean markers

	corruptAt(t, statePath, 8192) // SD-rot after a clean shutdown

	// Boot 1: clean marker present → boot check skipped → no heal yet.
	st1, err := Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if evs := st1.HealEvents(); len(evs) != 0 {
		t.Fatalf("clean-marker boot should skip the check, got heal events: %+v", evs)
	}
	// Background verify detects the rot and arms a heal for the next boot.
	st1.verifyOnce()
	st1.Close() // corrupt flagged → no clean marker written

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
