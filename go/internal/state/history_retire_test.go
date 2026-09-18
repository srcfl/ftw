package state

import (
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
