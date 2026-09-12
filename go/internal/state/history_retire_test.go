package state

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func sqliteLegacyHistoryTableCount(s *Store) int {
	n := 0
	for _, table := range historyTables {
		var c int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&c); err != nil {
			return -1
		}
		n += c
	}
	return n
}

func waitHistoryMigration(t *testing.T, s *Store) {
	t.Helper()
	if s.historyMigration == nil {
		return
	}
	select {
	case <-s.historyMigration.done:
	case <-time.After(15 * time.Second):
		t.Fatal("import did not finish")
	}
}

func TestOpenLeavesSqliteHistoryForFixtureBuilders(t *testing.T) {
	s := freshStore(t)
	if n := sqliteLegacyHistoryTableCount(s); n != len(historyTables) {
		t.Fatalf("Open dropped fixture tables: %d", n)
	}
}

func TestVerifiedImportDropsSqliteAndParquetAndHidesLaterBoots(t *testing.T) {
	path, cold := legacyMigrationFixture(t, 11)
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(day, "01.parquet")
	if err := writeParquetDay(file, []parquetSampleRow{{TsMs: 5000, Driver: "meter", Metric: "power", Value: 42}}); err != nil {
		t.Fatal(err)
	}
	diag := filepath.Join(cold, "diagnostics", "2026", "01")
	if err := os.MkdirAll(diag, 0700); err != nil {
		t.Fatal(err)
	}
	diagFile := filepath.Join(diag, "01.parquet")
	if err := os.WriteFile(diagFile, []byte("planner diagnostics"), 0600); err != nil {
		t.Fatal(err)
	}

	s, err := OpenWithBackgroundHistory(path, cold, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitHistoryMigration(t, s)
	if !s.HistoryMigrationStatus().HistoryComplete {
		t.Fatalf("import failed: %+v", s.HistoryMigrationStatus())
	}
	if n := sqliteLegacyHistoryTableCount(s); n != 0 {
		t.Fatalf("sqlite history still present: %d tables", n)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("imported parquet kept: %v", err)
	}
	if _, err := os.Stat(diagFile); err != nil {
		t.Fatalf("diagnostics parquet removed: %v", err)
	}
	got, err := s.LoadSeries("meter", "power", 0, 10000, 0)
	if err != nil || len(got) != 12 {
		t.Fatalf("duckdb history lost: %d %v", len(got), err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	var incomplete atomic.Int32
	s, err = OpenWithBackgroundHistory(path, cold, func(st HistoryMigrationStatus) {
		if !st.HistoryComplete {
			incomplete.Add(1)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.historyMigration != nil {
		t.Fatal("later boot started a history import")
	}
	if incomplete.Load() != 0 {
		t.Fatalf("later boot reported incomplete import %d times", incomplete.Load())
	}
	if !s.HistoryMigrationStatus().HistoryComplete {
		t.Fatal("later boot did not report complete history")
	}
	if n := sqliteLegacyHistoryTableCount(s); n != 0 {
		t.Fatalf("later boot recreated sqlite history: %d tables", n)
	}
	got, err = s.LoadSeries("meter", "power", 0, 10000, 0)
	if err != nil || len(got) != 12 {
		t.Fatalf("later boot lost duckdb history: %d %v", len(got), err)
	}
	if _, err := os.Stat(diagFile); err != nil {
		t.Fatalf("later boot removed diagnostics: %v", err)
	}
}

func TestRetiredHistoryBackupRestoresThroughPortableSQLite(t *testing.T) {
	path, cold := legacyMigrationFixture(t, 5)
	s, err := OpenWithBackgroundHistory(path, cold, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitHistoryMigration(t, s)
	if n := sqliteLegacyHistoryTableCount(s); n != 0 {
		s.Close()
		t.Fatalf("expected retired sqlite history, got %d tables", n)
	}
	gz := filepath.Join(filepath.Dir(path), "full.gz")
	if err := s.BackupToCompressed(gz); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	in, err := os.Open(gz)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	restored := filepath.Join(t.TempDir(), "restored.db")
	out, err := os.Create(restored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, zr); err != nil {
		out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.LoadSeries("meter", "power", 0, 100, 0)
	if err != nil || len(got) != 5 {
		t.Fatalf("portable restore lost history: %d %v", len(got), err)
	}
}

func TestOpenWithLegacyHistoryRetiresAfterSynchronousImport(t *testing.T) {
	dir := t.TempDir()
	cold := filepath.Join(dir, "cold")
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(day, "01.parquet")
	if err := writeParquetDay(file, []parquetSampleRow{{TsMs: 1, Driver: "meter", Metric: "power", Value: 9}}); err != nil {
		t.Fatal(err)
	}
	s, err := OpenWithLegacyHistory(filepath.Join(dir, "state.db"), cold)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := sqliteLegacyHistoryTableCount(s); n != 0 {
		t.Fatalf("offline import left sqlite history: %d tables", n)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("offline import kept parquet: %v", err)
	}
	got, err := s.LatestSample("meter", "power")
	if err != nil || got.Value != 9 {
		t.Fatalf("offline import lost duckdb history: %+v %v", got, err)
	}
}
