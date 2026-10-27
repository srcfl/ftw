package state

import (
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
