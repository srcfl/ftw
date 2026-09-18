package state

import (
	"os"
	"path/filepath"
	"testing"
)

// A frozen SQLite source beside a fresh DuckDB destination, as on first update.
func legacyMigrationFixture(t *testing.T, n int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	cold := filepath.Join(dir, "cold")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO ts_drivers(id,name) VALUES(7,'meter'); INSERT INTO ts_metrics(id,name,unit) VALUES(9,'power','W'); DELETE FROM config WHERE key LIKE 'history_%'`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO ts_samples(driver_id,metric_id,ts_ms,value) VALUES (7,9,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, err := stmt.Exec(i, float64(i)/7); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO energy_ledger_cursors VALUES ('site','import','counter',1234,10)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(historyDatabasePath(path)); err != nil {
		t.Fatal(err)
	}
	return path, cold
}
