package state

import (
	"compress/gzip"
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The update rollback point copies the settings database only. History
// stays in history.db, which a rollback leaves in place (#1302).
func TestBackupStateWithConfigurationLeavesHistoryInPlace(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveConfig("goal", "80%"); err != nil {
		t.Fatal(err)
	}
	if err := s.BulkRecordHistory(pacedBackupPoints(2500)); err != nil {
		t.Fatal(err)
	}
	if !s.HistoryInPlace() {
		t.Fatal("a fresh store must keep history in its own file")
	}
	live, err := s.historyConfig("history_sqlite_generation")
	if err != nil || live == "" {
		t.Fatalf("live history generation %q %v", live, err)
	}

	dst := filepath.Join(t.TempDir(), "state.db.gz")
	if _, _, err := s.BackupStateWithConfiguration(dst, nil); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(t.TempDir(), "state.db")
	gunzipRollbackPoint(t, dst, raw)
	db, err := sql.Open("sqlite", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var historyTablesInPoint int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('history_hot','history_warm','history_cold','ts_samples','ts_buckets')`).Scan(&historyTablesInPoint); err != nil {
		t.Fatal(err)
	}
	if historyTablesInPoint != 0 {
		t.Fatalf("rollback point copied %d history tables; history must stay in place", historyTablesInPoint)
	}
	var value string
	if err := db.QueryRow(`SELECT value FROM config WHERE key='goal'`).Scan(&value); err != nil || value != "80%" {
		t.Fatalf("rollback point lost settings: %q %v", value, err)
	}
	if err := db.QueryRow(`SELECT value FROM config WHERE key='history_sqlite_generation'`).Scan(&value); err != nil || value != live {
		t.Fatalf("rollback point must keep the live history generation %q, got %q %v", live, value, err)
	}
	var restore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM config WHERE key='history_restore_generation'`).Scan(&restore); err != nil || restore != 0 {
		t.Fatalf("rollback point must not claim an embedded history restore: %d %v", restore, err)
	}

	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&kept); err != nil || kept != 2500 {
		t.Fatalf("history.db rows after the point = %d %v, want 2500", kept, err)
	}
}

// The space preflight for the rollback point is bounded by the settings
// database, not by the history export a full backup needs.
func TestBackupStatePreflightIgnoresHistorySize(t *testing.T) {
	s := freshStore(t)
	if err := s.BulkRecordHistory(pacedBackupPoints(2500)); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.BackupSourceBytes() <= s.stateSourceBytes() {
		t.Fatal("history must add to the full backup source size")
	}
	orig := backupDiskAvail
	t.Cleanup(func() { backupDiskAvail = orig })
	need := backupCopyScratch(s.stateSourceBytes())

	backupDiskAvail = func(string) (int64, error) { return need, nil }
	if _, _, err := s.BackupStateWithConfiguration(filepath.Join(t.TempDir(), "point.gz"), nil); err != nil {
		t.Fatalf("rollback point refused with room for its own scratch: %v", err)
	}
	if err := s.BackupToCompressed(filepath.Join(t.TempDir(), "full.gz")); err == nil {
		t.Fatal("full backup accepted without room for the history export")
	}

	backupDiskAvail = func(string) (int64, error) { return need - 1, nil }
	if _, _, err := s.BackupStateWithConfiguration(filepath.Join(t.TempDir(), "short.gz"), nil); err == nil {
		t.Fatal("rollback point accepted without room for its scratch files")
	}
}

func gunzipRollbackPoint(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
