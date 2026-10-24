//go:build unix

package state

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestBackupScratchHolderHelper(t *testing.T) {
	dir := os.Getenv("FTW_BACKUP_SCRATCH_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	HoldBackupScratch(dir)
	if err := os.WriteFile(filepath.Join(dir, "ftw-backup-2.raw"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	fmt.Println("BACKUP_SCRATCH_HELD")
	<-time.After(time.Minute)
}

// On the home box systemd killed Core mid-backup, so its deferred cleanup
// never ran and the raw copy stayed in /dev/shm.
func TestKilledBackupScratchIsRemovedAndLiveScratchKept(t *testing.T) {
	dir := t.TempDir()
	notOurs := []string{"ftw-backup-x.raw", "ftw-backup-3.raw.bak", "other.raw", "ftw-backup-4.raw-wal"}
	for _, name := range notOurs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "ftw-backup-5.raw"), 0o700); err != nil {
		t.Fatal(err)
	}
	notOurs = append(notOurs, "ftw-backup-5.raw")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackupScratchHolderHelper$")
	cmd.Env = append(os.Environ(), "FTW_BACKUP_SCRATCH_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	held := false
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		if lines.Text() == "BACKUP_SCRATCH_HELD" {
			held = true
			break
		}
	}
	if !held {
		cmd.Wait()
		t.Fatal("helper never held the folder", ctx.Err(), lines.Err())
	}
	raw := filepath.Join(dir, "ftw-backup-2.raw")
	journal := raw + "-journal"
	if err := os.WriteFile(journal, make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}

	if removed, _ := RemoveStaleBackupScratch(dir, isShmBackupScratch); removed != 0 {
		t.Fatalf("removed %d entries of a running backup", removed)
	}
	for _, path := range []string{raw, journal} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("running backup lost %s: %v", path, err)
		}
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper exited cleanly")
	}
	logs := captureDefaultLog(t)
	removed, n := RemoveStaleBackupScratch(dir, isShmBackupScratch)
	if removed != 2 || n != 4096+512 {
		t.Fatalf("removed %d entries, %d bytes; want 2, 4608", removed, n)
	}
	for _, path := range []string{raw, journal} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s left after its backup was killed: %v", path, err)
		}
	}
	if got := logs.String(); !strings.Contains(got, "path="+raw+" bytes=4096") {
		t.Fatalf("log does not name the removed copy and its size:\n%s", got)
	}
	for _, name := range notOurs {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("removed %s, which is not backup scratch: %v", name, err)
		}
	}
}

// The raw copy is the large scratch file. On the home box it sat in /dev/shm.
func TestRawCopyFolderIsHeldWhileCopying(t *testing.T) {
	s := freshStore(t)
	dst := filepath.Join(t.TempDir(), "database.gz")
	held := 0
	err := s.backupToCompressed(dst, func(BackupProgress) {
		raw := ownRawCopy(dst)
		if raw == "" {
			return
		}
		lock, err := lockBackupScratchDir(filepath.Dir(raw), true)
		if err == nil {
			lock.Close()
			t.Errorf("nothing holds %s while %s is written", filepath.Dir(raw), raw)
			return
		}
		held++
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if held == 0 {
		t.Fatal("no progress report while the raw copy existed")
	}
}

// ownRawCopy finds this process's raw copy beside dst or, on Linux, among its
// open files in /dev/shm.
func ownRawCopy(dst string) string {
	if _, err := os.Stat(dst + ".raw.tmp"); err == nil {
		return dst + ".raw.tmp"
	}
	links, _ := filepath.Glob("/proc/self/fd/*")
	for _, link := range links {
		target, err := os.Readlink(link)
		if err == nil && strings.HasPrefix(target, "/dev/shm/ftw-backup-") && strings.HasSuffix(target, ".raw") {
			return target
		}
	}
	return ""
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureDefaultLog(t *testing.T) *lockedBuffer {
	t.Helper()
	logs := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}
