package nativeupdate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpaceNeededIsThreeTimesTheCurrentRelease(t *testing.T) {
	root := t.TempDir()
	release(t, root, "v0.131.0")
	if err := os.WriteFile(filepath.Join(root, "releases", "v0.131.0", "ftw"), make([]byte, 1000), 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manager{Root: root}
	need, err := m.SpaceNeeded("v0.131.0")
	if err != nil {
		t.Fatal(err)
	}
	// ftw (1000) + three 4-byte files + the receipt.
	receipt, _ := os.Stat(filepath.Join(root, "releases", "v0.131.0", receiptFile))
	if want := 3 * (1000 + 3*4 + receipt.Size()); need != want {
		t.Fatalf("need %d, want %d", need, want)
	}
	if free, err := m.FreeBytes(); err != nil || free <= 0 {
		t.Fatalf("free %d %v", free, err)
	}
	if err := m.CheckSpace("v0.131.0"); err != nil {
		t.Fatalf("a small release fits: %v", err)
	}
	if err := m.CheckSpace("v0.130.0"); err == nil || !strings.Contains(err.Error(), "v0.130.0") {
		t.Fatalf("a missing current release: %v", err)
	}
}

func TestCheckSpaceRefusesWhenTheNextReleaseWouldNotFit(t *testing.T) {
	root := t.TempDir()
	release(t, root, "v0.131.0")
	// A sparse file makes the current release look 8 TiB large without
	// using the space, so three times it cannot be free here.
	binary, err := os.OpenFile(filepath.Join(root, "releases", "v0.131.0", "ftw"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer binary.Close()
	if err := binary.Truncate(1 << 43); err != nil {
		t.Skipf("file system has no sparse files: %v", err)
	}
	err = Manager{Root: root}.CheckSpace("v0.131.0")
	if err == nil || !strings.Contains(err.Error(), "not enough disk space in "+root) {
		t.Fatalf("an oversized release: %v", err)
	}
}
