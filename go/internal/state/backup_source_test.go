package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenBackupSourceCopiesWithoutRunningMigrations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")
	db, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE legacy_only(value TEXT); INSERT INTO legacy_only VALUES ('preserved')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := OpenBackupSource(path)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy.db.gz")
	if err := source.BackupToCompressed(dst); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cache.db")); !os.IsNotExist(err) {
		t.Fatalf("backup source created cache.db: %v", err)
	}
	if _, err := os.Stat(path + ".clean"); !os.IsNotExist(err) {
		t.Fatalf("backup source wrote clean marker: %v", err)
	}
}
