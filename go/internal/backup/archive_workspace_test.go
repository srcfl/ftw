package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
)

func TestReadOnlyArchiveCanBeVerifiedAndRestored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permissions need a POSIX filesystem")
	}
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveConfig("ev_goal", "80% by 07:00"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "read-only-source")
	info, err := Create(context.Background(), CreateOptions{State: st, StatePath: filepath.Join(data, "state.db"), DataDir: data, OutputDir: source})
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "temp")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", workspace)
	if err := os.Chmod(source, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(source, 0700) })
	if probe, err := os.MkdirTemp(source, "probe-"); err == nil {
		_ = os.Remove(probe)
		t.Skip("current user bypasses directory write permissions")
	}
	if _, err := Verify(info.Path); err != nil {
		t.Fatal("verify from read-only source:", err)
	}
	if entries, err := os.ReadDir(workspace); err != nil || len(entries) != 0 {
		t.Fatalf("verification left temporary files: %v %v", entries, err)
	}
	// Restore must use its target filesystem even when the normal temp
	// directory is unavailable. Neither operation may write at the source.
	t.Setenv("TMPDIR", filepath.Join(root, "missing-temp"))
	for _, inPlace := range []bool{false, true} {
		target := filepath.Join(root, "restored")
		if inPlace {
			target = filepath.Join(root, "mounted-target")
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			_, err = RestoreContents(info.Path, target, time.Time{})
		} else {
			_, err = Restore(info.Path, target, time.Time{})
		}
		if err != nil {
			t.Fatalf("restore inPlace=%v: %v", inPlace, err)
		}
		restored, err := state.Open(filepath.Join(target, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		goal, ok := restored.LoadConfig("ev_goal")
		_ = restored.Close()
		if !ok || goal != "80% by 07:00" {
			t.Fatalf("restore changed stored goal: %q %v", goal, ok)
		}
	}
}

func TestCreateChecksColdFilesBeforeExport(t *testing.T) {
	root := t.TempDir()
	st, err := state.Open(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cold := filepath.Join(root, "cold", "2026", "01", "01.parquet")
	writeTestParquet(t, cold)
	// A backup also carries pairing and driver files. Count actual bytes,
	// even when a file compresses well; free-space checks cannot assume that.
	if err := os.WriteFile(filepath.Join(root, "pairing.key"), []byte(strings.Repeat("k", 2<<20)), 0600); err != nil {
		t.Fatal(err)
	}
	coldInfo, err := os.Stat(cold)
	if err != nil {
		t.Fatal(err)
	}
	const pairedBytes = 2 << 20
	sourceBytes := st.BackupSourceBytes()
	oldCheck := ensureBackupDiskSpace
	t.Cleanup(func() { ensureBackupDiskSpace = oldCheck })
	full := errors.New("test filesystem too small")
	called := false
	output := filepath.Join(root, "backups")
	ensureBackupDiskSpace = func(dir string, needed int64) error {
		called = true
		minimum := state.BackupArchiveScratch(sourceBytes, pairedBytes+coldInfo.Size())
		if dir != output || needed < minimum {
			t.Fatalf("space check omits backup files: dir=%s need=%d minimum=%d", dir, needed, minimum)
		}
		return full
	}
	_, err = Create(context.Background(), CreateOptions{State: st, StatePath: filepath.Join(root, "state.db"), DataDir: root, OutputDir: output})
	if !called || !errors.Is(err, full) {
		t.Fatalf("export started without its complete space check: %v %v", called, err)
	}
	if files, err := os.ReadDir(output); err != nil || len(files) != 0 {
		t.Fatalf("failed preflight left partial export: %v %v", files, err)
	}
}
