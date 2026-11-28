package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedSnapshotEvents(t *testing.T, s *Store, rows int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	payload := strings.Repeat("x", 1024)
	for i := 1; i <= rows; i++ {
		if _, err := tx.Exec(`INSERT INTO events(ts_ms, event) VALUES (?, ?)`, i, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// #1554: one INSERT ... SELECT per table left the whole table dirty for one
// fsync. On an SD card live history commits waited behind it past their
// five-second deadline. The snapshot must commit short batches and yield.
func TestSnapshotYieldsBetweenShortSyncedBatches(t *testing.T) {
	s := freshStore(t)
	seedSnapshotEvents(t, s, 4096) // about 4 MiB in one table
	dst := filepath.Join(t.TempDir(), "snap.db")
	var pauses int
	var last, largest int64
	s.backupPause = func(context.Context) error {
		pauses++
		info, err := os.Stat(dst)
		if err != nil {
			return err
		}
		largest = max(largest, info.Size()-last)
		last = info.Size()
		return nil
	}
	if err := s.SnapshotTo(dst); err != nil {
		t.Fatal(err)
	}
	if pauses < 8 {
		t.Fatalf("snapshot yielded %d times while copying 4 MiB", pauses)
	}
	if largest > 1<<20 {
		t.Fatalf("snapshot wrote %d bytes between yields; want at most 1 MiB", largest)
	}
	db, err := openRaw(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil || n != 4096 {
		t.Fatalf("snapshot events = %d, err=%v", n, err)
	}
}

// Writes that land while the copy pauses belong to the next snapshot, in
// every table, so a restore never mixes two points in time.
func TestSnapshotCopiesOnePointInTime(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveConfig("goal", "before"); err != nil {
		t.Fatal(err)
	}
	seedSnapshotEvents(t, s, 1024)
	wrote := false
	s.backupPause = func(context.Context) error {
		if wrote {
			return nil
		}
		wrote = true
		// config is copied before events, telemetry after.
		if err := s.SaveConfig("goal", "after"); err != nil {
			return err
		}
		return s.SaveTelemetry("late", `{"w":1}`)
	}
	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(dst); err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("snapshot never yielded")
	}
	db, err := openRaw(dst)
	if err != nil {
		t.Fatal(err)
	}
	snap := &Store{db: db}
	defer snap.Close()
	if v, ok := snap.LoadConfig("goal"); !ok || v != "before" {
		t.Fatalf("snapshot goal = %q, %v; want the value when the copy began", v, ok)
	}
	if _, ok := snap.LoadTelemetry("late"); ok {
		t.Fatal("snapshot holds a row written after the copy began")
	}
	if v, ok := s.LoadConfig("goal"); !ok || v != "after" {
		t.Fatalf("live goal = %q, %v", v, ok)
	}
}

// The control journal uses AUTOINCREMENT. A restored snapshot must not hand
// out an id that a deleted row already had.
func TestSnapshotKeepsAutoincrementHighWaterMark(t *testing.T) {
	s := freshStore(t)
	for i := range 3 {
		if _, err := s.db.Exec(`INSERT INTO control_journal(subject,status,start_ms,updated_ms) VALUES('battery','following',?,?)`, i, i); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM control_journal WHERE id=3`); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(dst); err != nil {
		t.Fatal(err)
	}
	db, err := openRaw(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var seq int
	if err := db.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='control_journal'`).Scan(&seq); err != nil || seq != 3 {
		t.Fatalf("snapshot control_journal sequence = %d, err=%v; want 3", seq, err)
	}
}
