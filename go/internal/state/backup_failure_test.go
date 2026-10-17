package state

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The home box's backup on 16 October 2026 copied on the SD card, ran into
// the live time limit and reported only "context deadline exceeded".
func TestLiveBackupPastItsLimitNamesTheLimitAndTable(t *testing.T) {
	s := freshStore(t)
	if err := s.BulkRecordHistory(pacedBackupPoints(5000)); err != nil {
		t.Fatal(err)
	}
	orig := liveBackupTimeout
	t.Cleanup(func() { liveBackupTimeout = orig })
	liveBackupTimeout = time.Second
	// Each batch waits as a commit on the SD card did; four outlast the limit.
	s.backupPause = func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
			return nil
		}
	}
	err := s.BackupToCompressed(filepath.Join(t.TempDir(), "full.gz"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backup past its limit = %v", err)
	}
	for _, want := range []string{"live backup reached its time limit of 1s", "copy history_hot"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("reason %q does not say %q", err, want)
		}
	}
}

func TestCanceledBackupIsNotBlamedOnTheLimit(t *testing.T) {
	s := freshStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := s.BackupWithConfigurationContext(ctx, filepath.Join(t.TempDir(), "full.gz"), nil)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "time limit") {
		t.Fatalf("canceled backup = %v", err)
	}
}

func TestBackupScratchOnDiskNamesScratchLeftInShm(t *testing.T) {
	shm := t.TempDir()
	orig := backupShmDir
	t.Cleanup(func() { backupShmDir = orig })
	backupShmDir = shm
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	path, cleanup := backupScratchFile(filepath.Join(t.TempDir(), "full.gz"), 1<<20)
	cleanup()
	if !strings.HasPrefix(path, shm) || logs.Len() != 0 {
		t.Fatalf("fitting scratch = %s, logs %q", path, logs.String())
	}

	// A backup stopped by a restart leaves its scratch until reboot.
	if err := os.WriteFile(filepath.Join(shm, "ftw-backup-4218622654.raw"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "full.gz")
	path, cleanup = backupScratchFile(dst, 1<<50)
	defer cleanup()
	if path != dst+".raw.tmp" {
		t.Fatalf("oversized scratch = %s", path)
	}
	for _, want := range []string{"copying on the data disk", "backup_scratch_in_shm_bytes=4096"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log %q does not say %q", logs.String(), want)
		}
	}
}
