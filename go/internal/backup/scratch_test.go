//go:build unix

package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestStaleScratchIsRemovedAndArchivesKept(t *testing.T) {
	out := t.TempDir()
	writeTestFile(t, filepath.Join(out, ".ftw-backup-stage-123", "database.gz.raw.tmp"), strings.Repeat("r", 3000))
	writeTestFile(t, filepath.Join(out, ".ftw-backup-verify-456", "database.gz"), strings.Repeat("v", 1000))
	writeTestFile(t, filepath.Join(out, ".ftw-full-backup-20261013T103000.000Z.ftwbak.tmp"), strings.Repeat("a", 2000))
	keep := []string{
		"ftw-full-backup-20261010T020000.000Z.ftwbak",
		"ftw-full-backup-20261010T020000.000Z.ftwbak.verified.json",
		"ftw-full-backup-20261010T020000.000Z.ftwbak.verified.json.tmp",
		".ftw-backup-stage-x/database.gz",
		".ftw-pre-restore-20261010T020000Z/state.db",
		"notes.txt",
	}
	for _, name := range keep {
		writeTestFile(t, filepath.Join(out, name), "keep")
	}

	removed, bytes := state.RemoveStaleBackupScratch(out, isScratch)
	if removed != 3 || bytes != 6000 {
		t.Fatalf("removed %d entries, %d bytes; want 3, 6000", removed, bytes)
	}
	for _, name := range []string{".ftw-backup-stage-123", ".ftw-backup-verify-456", ".ftw-full-backup-20261013T103000.000Z.ftwbak.tmp"} {
		if _, err := os.Stat(filepath.Join(out, name)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", name, err)
		}
	}
	for _, name := range keep {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Fatalf("removed %s: %v", name, err)
		}
	}
}

func TestRunningBackupKeepsItsScratch(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dataDir, "state.db")
	st, err := state.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	out := filepath.Join(root, "backups")
	seen := map[string]bool{}
	// A Core starting beside this backup sweeps at every step of it.
	sweep := func(state.BackupProgress) {
		if removed, _ := state.RemoveStaleBackupScratch(out, isScratch); removed != 0 {
			t.Errorf("removed %d entries of a running backup", removed)
		}
		entries, _ := os.ReadDir(out)
		for _, entry := range entries {
			if isScratch(entry) {
				seen[strings.TrimRight(entry.Name(), "0123456789")] = true
			}
		}
	}
	info, err := Create(context.Background(), CreateOptions{
		State: st, StatePath: statePath, DataDir: dataDir, OutputDir: out, Progress: sweep,
	})
	if err != nil {
		t.Fatal(err)
	}
	tmp := false
	for name := range seen {
		tmp = tmp || strings.HasSuffix(name, ".ftwbak.tmp")
	}
	if !seen[".ftw-backup-stage-"] || !tmp {
		t.Fatalf("no sweep ran while the stage folder and archive temp existed; saw %v", seen)
	}

	// Verifying a finished backup, as POST /api/backups/{id}/verify does.
	clear(seen)
	if _, err := verifyInWorkspaceProgressContext(context.Background(), info.Path, "", sweep); err != nil {
		t.Fatal(err)
	}
	if !seen[".ftw-backup-verify-"] {
		t.Fatalf("no sweep ran while the verification folder existed; saw %v", seen)
	}
}
