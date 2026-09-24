package backup

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The state snapshot is rewritten through a temp file while a backup walks
// the data dir; on the home box that failed a 15-minute backup with
// "lstat state.db.snapshot.tmp: no such file". Neither it nor cache.db
// belongs in the archive.
func TestCollectSourcesSkipsDerivedStateFiles(t *testing.T) {
	dataDir := t.TempDir()
	for _, name := range []string{
		"config.yaml", "webpush.key",
		"state.db", "state.db-wal", "state.db-shm",
		"state.db.snapshot", "state.db.snapshot.tmp",
		"cache.db", "cache.db-wal", "cache.db-shm",
	} {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := collectSources(dataDir, filepath.Join(dataDir, "state.db"), filepath.Join(dataDir, "backups"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range sources {
		got = append(got, s.archivePath)
	}
	sort.Strings(got)
	want := []string{"data/config.yaml", "data/webpush.key"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("collected %v, want %v", got, want)
	}
}
