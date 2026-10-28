package state

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func seedHistoryStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// reopenHealed opens a store whose history.db was damaged or removed and
// checks that Core starts with an empty, bound history and reports the loss.
func reopenHealed(t *testing.T, path string, wantQuarantine bool) {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open with damaged history.db failed: %v", err)
	}
	defer s.Close()
	events := s.HealEvents()
	if len(events) != 1 || events[0].Tier != tierHistory || events[0].Action != healRebuilt {
		t.Fatalf("heal events = %+v, want one history rebuilt event", events)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: 2}); err != nil {
		t.Fatalf("fresh history does not accept writes: %v", err)
	}
	active, err := s.historyConfig("history_sqlite_generation")
	if err != nil || active == "" {
		t.Fatalf("fresh history is not bound: %q %v", active, err)
	}
	moved, _ := filepath.Glob(historyDatabasePath(path) + ".corrupt-*")
	if wantQuarantine && len(moved) == 0 {
		t.Fatal("damaged history.db was not kept for manual recovery")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("second open after heal: %v", err)
	}
	if n := len(again.HealEvents()); n != 0 {
		t.Fatalf("second open reported %d heal events, want 0", n)
	}
	again.Close()
}

// A torn history.db must not stop Core from opening.
func TestCorruptHistoryHealsOnOpen(t *testing.T) {
	path := seedHistoryStore(t)
	hist := historyDatabasePath(path)
	f, err := os.OpenFile(hist, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Overwrite the 100-byte SQLite header (torn page 1).
	if _, err := f.WriteAt(make([]byte, 100), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	os.Remove(hist + "-wal")
	os.Remove(hist + "-shm")
	reopenHealed(t, path, true)
}

// Removing history.db by hand must not stop Core from opening.
func TestMissingHistoryHealsOnOpen(t *testing.T) {
	path := seedHistoryStore(t)
	hist := historyDatabasePath(path)
	for _, p := range []string{hist, hist + "-wal", hist + "-shm"} {
		os.Remove(p)
	}
	reopenHealed(t, path, false)
}

// A readable history.db from another generation is a wrong file, not damage.
func TestForeignHistoryGenerationStillRefused(t *testing.T) {
	path := seedHistoryStore(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("history_sqlite_generation", "unrelated"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(path); err == nil {
		reopened.Close()
		t.Fatal("accepted a history.db bound to another generation")
	}
}

// corruptTablePage overwrites the root page of one table inside history.db.
func corruptTablePage(t *testing.T, hist, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", hist)
	if err != nil {
		t.Fatal(err)
	}
	var root, pageSize int64
	if err := db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE name=?`, table).Scan(&root); err != nil {
		t.Fatalf("rootpage %s: %v", table, err)
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	os.Remove(hist + "-wal")
	os.Remove(hist + "-shm")
	f, err := os.OpenFile(hist, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	junk := make([]byte, pageSize)
	for i := range junk {
		junk[i] = 0xA5
	}
	if _, err := f.WriteAt(junk, (root-1)*pageSize); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// A damaged data page read by a later startup query must heal too, not only
// damage seen by the schema step.
func TestCorruptHistoryDataPageHealsOnOpen(t *testing.T) {
	for _, table := range []string{"history_source_cursor", "energy_ledger_meta"} {
		t.Run(table, func(t *testing.T) {
			path := seedHistoryStore(t)
			corruptTablePage(t, historyDatabasePath(path), table)
			reopenHealed(t, path, true)
		})
	}
}

// A torn half-size file heals.
func TestTruncatedHistoryHealsOnOpen(t *testing.T) {
	path := seedHistoryStore(t)
	hist := historyDatabasePath(path)
	os.Remove(hist + "-wal")
	os.Remove(hist + "-shm")
	fi, err := os.Stat(hist)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(hist, fi.Size()/2); err != nil {
		t.Fatal(err)
	}
	reopenHealed(t, path, true)
}

// SQLite opens a 0-byte file as an empty database. With no marker it cannot
// be a wrong file from a restore, so it heals like a missing file.
func TestZeroLengthHistoryHealsOnOpen(t *testing.T) {
	path := seedHistoryStore(t)
	hist := historyDatabasePath(path)
	os.Remove(hist + "-wal")
	os.Remove(hist + "-shm")
	if err := os.Truncate(hist, 0); err != nil {
		t.Fatal(err)
	}
	reopenHealed(t, path, true)
}

// convertedBetaStore marks a seeded site as one that converted beta history.
func convertedBetaStore(t *testing.T) string {
	t.Helper()
	path := seedHistoryStore(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.historyConfig("history_sqlite_generation")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("history_duckdb_generation", active); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`INSERT INTO history_migrations(name) VALUES (?)`, convertedChartSummaries); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A site that already converted beta history keeps history_duckdb_generation.
// Its history must heal like any other, and the fresh file must not be taken
// for beta history that still needs conversion.
func TestConvertedBetaHistoryHealsOnOpen(t *testing.T) {
	damage := map[string]func(t *testing.T, hist string){
		"missing": func(t *testing.T, hist string) {
			for _, p := range []string{hist, hist + "-wal", hist + "-shm"} {
				os.Remove(p)
			}
		},
		"header": func(t *testing.T, hist string) {
			os.Remove(hist + "-wal")
			os.Remove(hist + "-shm")
			f, err := os.OpenFile(hist, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt(make([]byte, 100), 0); err != nil {
				t.Fatal(err)
			}
			f.Close()
		},
		"data page": func(t *testing.T, hist string) { corruptTablePage(t, hist, "energy_ledger_meta") },
		"zero length": func(t *testing.T, hist string) {
			os.Remove(hist + "-wal")
			os.Remove(hist + "-shm")
			if err := os.Truncate(hist, 0); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, apply := range damage {
		t.Run(name, func(t *testing.T) {
			path := convertedBetaStore(t)
			apply(t, historyDatabasePath(path))
			reopenHealed(t, path, name != "missing")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if beta, err := s.historyConfig("history_duckdb_generation"); err != nil || beta != "" {
				t.Fatalf("beta key after heal = %q, %v; want cleared", beta, err)
			}
		})
	}
}

// The restore path keeps refusing damage: the restored file is the only copy.
func TestRestoreGenerationCorruptStillRefused(t *testing.T) {
	path := seedHistoryStore(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := s.historyConfig("history_sqlite_generation")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("history_restore_generation", gen); err != nil {
		t.Fatal(err)
	}
	s.Close()
	hist := historyDatabasePath(path)
	os.Remove(hist + "-wal")
	os.Remove(hist + "-shm")
	f, err := os.OpenFile(hist, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt(make([]byte, 100), 0)
	f.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("restore path healed a damaged history.db")
	}
}
