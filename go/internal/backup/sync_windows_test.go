package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsSyncDirectoryRejectsMissingAndRegularPaths(t *testing.T) {
	dir := t.TempDir()
	if err := syncDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := syncDir(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing directory: %v", err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncDir(file); err == nil {
		t.Fatal("treated a regular file as a directory")
	}
}
