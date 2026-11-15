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
	quarantined := false
	for _, entry := range entries {
		if strings.Contains(entry.Name(), "forecast-archive.db.corrupt-") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Fatal("damaged archive was not moved aside")
	}
}
