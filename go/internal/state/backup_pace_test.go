package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackupWorkContextOmitsDeadlineWhenOffline(t *testing.T) {
	offline := &Store{offlineBackup: true}
	ctx, cancel := offline.backupWorkContext()
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("offline backup must not inherit the live export deadline")
	}

	live := &Store{}
	ctx, cancel = live.backupWorkContext()
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("live backup must keep an export deadline")
	}
	until := time.Until(deadline)
	if until > liveBackupTimeout || until < liveBackupTimeout-time.Minute {
		t.Fatalf("live backup deadline = %v", until)
	}
}

func TestLiveBackupCopyYieldsBetweenBatches(t *testing.T) {
	s := freshStore(t)
	pauses := 0
	s.backupPause = func(context.Context) error { pauses++; return nil }
	if err := s.BulkRecordHistory(pacedBackupPoints(2500)); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "export.db")
	if err := s.copyStateForBackup(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	if err := s.exportHistoryToSQLite(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	if pauses == 0 {
		t.Fatal("live backup must yield between copy batches")
	}
}

func TestOfflineBackupCopyDoesNotYieldBetweenBatches(t *testing.T) {
	s := freshStore(t)
	if err := s.BulkRecordHistory(pacedBackupPoints(2500)); err != nil {
		t.Fatal(err)
	}
	path := s.mainDBPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	src, err := OpenBackupSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if !src.OfflineBackup() {
		t.Fatal("OpenBackupSource must mark the helper offline")
	}
	pauses := 0
	src.backupPause = func(context.Context) error {
		pauses++
		return nil
	}
	dst := filepath.Join(t.TempDir(), "export.db")
	if err := src.copyStateForBackup(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	if err := src.exportHistoryToSQLite(context.Background(), dst); err != nil {
		t.Fatal(err)
	}
	if pauses != 0 {
		t.Fatalf("offline backup paused %d times; live 100ms yield must not apply", pauses)
	}
	db, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&n); err != nil || n != 2500 {
		t.Fatalf("offline export lost rows: %d %v", n, err)
	}
}

func TestBackupArchiveScratchReservesRawGzipAndVerify(t *testing.T) {
	const source, extra int64 = 100, 7
	got := BackupArchiveScratch(source, extra)
	if got < 4*source+2*extra+backupScratchHeadroom {
		t.Fatalf("scratch %d omits a coexistence phase", got)
	}
	// Issue #1259: 174,629,397 sample rows with a 100 ms pause every 1,024
	// rows spend 4h44m paused, so the live 2h export deadline must fire.
	const rows int64 = 174629397
	livePause := time.Duration(rows/1024) * maintenancePause
	if livePause <= liveBackupTimeout {
		t.Fatalf("live pause floor %v no longer exceeds the %v export deadline", livePause, liveBackupTimeout)
	}
	if backupCopyScratch(source) < 2*source {
		t.Fatal("copy scratch must hold raw export and gzip together")
	}
}

func TestEnsureDiskSpaceRejectsShortFilesystem(t *testing.T) {
	orig := backupDiskAvail
	t.Cleanup(func() { backupDiskAvail = orig })
	needed := BackupArchiveScratch(100, 0)
	backupDiskAvail = func(string) (int64, error) { return needed - 1, nil }
	if err := EnsureDiskSpace(t.TempDir(), needed); err == nil {
		t.Fatal("accepted a filesystem smaller than the export scratch")
	}
	backupDiskAvail = func(string) (int64, error) { return needed, nil }
	if err := EnsureDiskSpace(t.TempDir(), needed); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDiskSpaceSkipsUnknownProbe(t *testing.T) {
	orig := backupDiskAvail
	t.Cleanup(func() { backupDiskAvail = orig })
	backupDiskAvail = func(string) (int64, error) { return 0, errors.New("unsupported") }
	if err := EnsureDiskSpace(t.TempDir(), 1<<40); err != nil {
		t.Fatal(err)
	}
}

func TestBackupScratchUsesTmpfsWhenItFits(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("no tmpfs")
	}
	path, cleanup := backupScratchFile(filepath.Join(t.TempDir(), "full.gz"), 1<<20)
	defer cleanup()
	if !strings.HasPrefix(path, "/dev/shm/") {
		t.Fatalf("scratch stayed on the data disk: %s", path)
	}
}

func TestBackupScratchStaysOnDiskWhenTmpfsIsTooSmall(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "full.gz")
	path, cleanup := backupScratchFile(dst, 1<<40)
	defer cleanup()
	if path != dst+".raw.tmp" {
		t.Fatalf("oversized scratch = %s", path)
	}
}

func TestBackupToCompressedPreflightUsesSourceSize(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveConfig("goal", "80%"); err != nil {
		t.Fatal(err)
	}
	orig := backupDiskAvail
	t.Cleanup(func() { backupDiskAvail = orig })
	backupDiskAvail = func(string) (int64, error) { return 1, nil }
	err := s.BackupToCompressed(filepath.Join(t.TempDir(), "full.gz"))
	if err == nil {
		t.Fatal("published a backup without room for scratch files")
	}
}

func pacedBackupPoints(n int) []HistoryPoint {
	points := make([]HistoryPoint, n)
	for i := range points {
		points[i] = HistoryPoint{TsMs: int64(i + 1), GridW: float64(i)}
	}
	return points
}
