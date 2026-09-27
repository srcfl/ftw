package backup

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// The state snapshot is rewritten through a temp database and its journal
// while a backup walks the data dir; on the home box that failed 15-minute
// backups with "lstat state.db.snapshot.tmp: no such file" and then
// ".snapshot.tmp-journal". None of it belongs in the archive. The live
// cache.db files are skipped too; Create archives a consistent copy instead.
func TestCollectSourcesSkipsDerivedStateFiles(t *testing.T) {
	dataDir := t.TempDir()
	for _, name := range []string{
		"config.yaml", "webpush.key",
		"state.db", "state.db-wal", "state.db-shm", "state.db-journal",
		"state.db.snapshot", "state.db.snapshot.tmp", "state.db.snapshot.tmp-journal",
		"state.db.snapshot.tmp-wal", "state.db.snapshot.tmp-shm",
		"cache.db", "cache.db-wal", "cache.db-shm", "cache.db-journal",
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
