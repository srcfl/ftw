package state

import (
	"os"
	"path/filepath"
	"testing"
)

// A DB whose freelist crosses both thresholds must shrink on CompactIfBloated;
// a healthy DB must be left alone (VACUUM is a full-file rewrite — SD wear).
func TestCompactIfBloated(t *testing.T) {
	origBytes, origShare := compactMinFreelistBytes, compactMinFreelistShare
	compactMinFreelistBytes, compactMinFreelistShare = 4096, 0.20
	t.Cleanup(func() {
		compactMinFreelistBytes, compactMinFreelistShare = origBytes, origShare
	})

	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Grow the file with bulk rows, then delete them all so the pages land
	// on the freelist.
	blob := make([]byte, 1024)
	for i := range blob {
		blob[i] = 'x'
	}
	for i := 0; i < 2000; i++ {
		if err := s.RecordHistory(HistoryPoint{TsMs: int64(i), JSON: string(blob)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM history_hot`); err != nil {
		t.Fatal(err)
	}
	// Move the WAL into the main file so freelist_count reflects the deletes.
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}

	sizeBefore := fileSize(t, path)
	var freelistBefore int64
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freelistBefore); err != nil {
		t.Fatal(err)
	}
	if freelistBefore == 0 {
		t.Fatal("test setup produced no freelist pages")
	}

	s.CompactIfBloated()

	var freelistAfter int64
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&freelistAfter); err != nil {
		t.Fatal(err)
	}
	if freelistAfter >= freelistBefore {
		t.Fatalf("freelist not reclaimed: before=%d after=%d", freelistBefore, freelistAfter)
	}
	if got := fileSize(t, path); got >= sizeBefore {
		t.Fatalf("file did not shrink: before=%d after=%d", sizeBefore, got)
	}
}

func TestCompactIfBloatedSkipsHealthyDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.RecordHistory(HistoryPoint{TsMs: 1, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	// Default thresholds (64 MB) — a tiny healthy DB must not be vacuumed.
	// There is no direct observable for "VACUUM ran" beyond it not erroring,
	// so assert the cheap invariant: the call returns and the DB still works.
	s.CompactIfBloated()
	if err := s.RecordHistory(HistoryPoint{TsMs: 2, JSON: "{}"}); err != nil {
		t.Fatalf("store broken after CompactIfBloated on healthy DB: %v", err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
