package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForecastLongArchiveRebuildsCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	archivePath := ForecastLongArchivePath(path)
	if err := os.WriteFile(archivePath, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Copies left by earlier damage. Only the newest quarantine stays.
	stale := []string{"forecast-archive.db.corrupt-1", "forecast-archive.db-wal.corrupt-1", "forecast-archive.db-shm.corrupt-2"}
	others := []string{"cache.db.corrupt-1", "forecast-archive.db.corrupted"}
	for _, name := range append(append([]string(nil), stale...), others...) {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, 4096), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("state.Open failed because of the analytics file: %v", err)
	}
	defer reopened.Close()
	if events := reopened.HealEvents(); len(events) != 0 {
		t.Fatalf("opening state healed the archive on the boot path: %+v", events)
	}
	arch, err := reopened.OpenForecastLongArchive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events := reopened.HealEvents()
	if len(events) != 1 || events[0].Tier != "forecast_archive" || events[0].Action != healRebuilt {
		t.Fatalf("heal events = %+v", events)
	}
	var runs int
	if err := arch.db.QueryRow(`SELECT COUNT(*) FROM fa_runs`).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("rebuilt archive runs = %d err=%v", runs, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var quarantined []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "forecast-archive.db") && strings.Contains(entry.Name(), ".corrupt-") {
			quarantined = append(quarantined, entry.Name())
		}
	}
	if len(quarantined) != 1 || quarantined[0] == stale[0] || !strings.HasPrefix(quarantined[0], "forecast-archive.db.corrupt-") {
		t.Fatalf("quarantined archive files = %v, want only the new copy", quarantined)
	}
	for _, name := range others {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("unrelated file %s was removed: %v", name, err)
		}
	}
}
