package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// closedLongArchive leaves forecast-archive.db on disk with one run and
// returns a fresh store that has not opened it.
func closedLongArchive(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	arch, err := store.OpenForecastLongArchive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	issue := longIssue("kept", time.Now().UTC().Truncate(time.Hour).Add(-time.Hour))
	if err := arch.ArchiveIssues(context.Background(), []ForecastArchiveIssue{{Issue: issue}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	return reopened, ForecastLongArchivePath(path)
}

func quarantinedFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".corrupt-") {
			out = append(out, entry.Name())
		}
	}
	return out
}

func TestForecastLongOpenDoesNotBlockStatusReaders(t *testing.T) {
	store, archivePath := closedLongArchive(t)
	// Another connection holds the file, so the open waits in SQLite's busy
	// handler as it would behind a long quick_check on an SD card.
	lock, err := sql.Open("sqlite", archivePath+"?_pragma=locking_mode(EXCLUSIVE)")
	if err != nil {
		t.Fatal(err)
	}
	lock.SetMaxOpenConns(1)
	defer lock.Close()
	if _, err := lock.Exec(`INSERT OR REPLACE INTO fa_meta(key, value) VALUES('lock', 'held')`); err != nil {
		t.Fatal(err)
	}
	opened := make(chan error, 1)
	go func() {
		_, err := store.OpenForecastLongArchive(context.Background())
		opened <- err
	}()
	for store.forecastLongOpenMu.TryLock() {
		store.forecastLongOpenMu.Unlock()
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	readers := make(chan error, 1)
	go func() {
		store.ReportForecastLongArchive(ForecastLongArchiveStatus{State: "starting"})
		if _, ok := store.ForecastLongArchiveStatus(); !ok {
			readers <- errors.New("status missing")
			return
		}
		_, err := store.SQLiteInventory(context.Background())
		readers <- err
	}()
	select {
	case err := <-readers:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("health status and storage inventory waited for the archive open")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-opened; err != nil {
		t.Fatalf("open after the lock was released: %v", err)
	}
	if files := quarantinedFiles(t, filepath.Dir(archivePath)); len(files) != 0 {
		t.Fatalf("a locked archive was moved aside: %v", files)
	}
}

func TestForecastLongOpenKeepsFileOnReadError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	store, archivePath := closedLongArchive(t)
	if err := os.Chmod(archivePath, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(archivePath, 0o600) })
	if _, err := store.OpenForecastLongArchive(context.Background()); err == nil {
		t.Fatal("unreadable archive opened")
	}
	if files := quarantinedFiles(t, filepath.Dir(archivePath)); len(files) != 0 {
		t.Fatalf("a read error moved a good archive aside: %v", files)
	}
	if events := store.HealEvents(); len(events) != 0 {
		t.Fatalf("heal events = %+v", events)
	}
	if err := os.Chmod(archivePath, 0o600); err != nil {
		t.Fatal(err)
	}
	arch, err := store.OpenForecastLongArchive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := arch.LoadIssue(context.Background(), "kept"); err != nil {
		t.Fatalf("the next open lost the stored run: %v", err)
	}
}

func TestForecastLongSyncDropsDamagedArchive(t *testing.T) {
	store, arch := openLongArchive(t)
	ctx := context.Background()
	if err := store.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	issue := longIssue("before", time.Now().UTC().Truncate(time.Hour).Add(-time.Hour))
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: issue}}); err != nil {
		t.Fatal(err)
	}
	// Each query takes a new connection, which reads the damaged header.
	arch.db.SetMaxIdleConns(0)
	if _, err := arch.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arch.path, make([]byte, 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	err := arch.Sync(ctx, store, time.Now().UTC())
	if !sqliteCorrupt(err) {
		t.Fatalf("sync err = %v, want a corruption code", err)
	}
	if err := arch.db.Ping(); err == nil {
		t.Fatal("damaged handle is still open")
	}
	healed, err := store.OpenForecastLongArchive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if healed == arch {
		t.Fatal("open returned the dropped handle")
	}
	events := store.HealEvents()
	if len(events) != 1 || events[0].Tier != "forecast_archive" {
		t.Fatalf("heal events = %+v", events)
	}
	if err := healed.Sync(ctx, store, time.Now().UTC()); err != nil {
		t.Fatalf("copy into the rebuilt archive: %v", err)
	}
	if files := quarantinedFiles(t, filepath.Dir(arch.path)); len(files) == 0 {
		t.Fatal("damaged archive was not moved aside")
	}
	// A corruption code read from state.db is not damage in the archive.
	corrupt := sqliteErrorOf(t)
	if !forecastLongDamaged(fmt.Errorf("copy: %w", corrupt)) {
		t.Fatal("archive corruption was not recognised")
	}
	if forecastLongDamaged(fmt.Errorf("copy: %w", fromForecastSource(corrupt))) {
		t.Fatal("a damaged state.db would drop the archive")
	}
}

// sqliteErrorOf returns a real SQLITE_NOTADB error.
func sqliteErrorOf(t *testing.T) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "junk.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database at all, just text"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`SELECT * FROM sqlite_master`)
	if !sqliteCorrupt(err) {
		t.Fatalf("junk file error = %v", err)
	}
	return err
}

// A corruption code that quick_check would not see still heals the file, and
// the rebuild waits for the last read on the dropped handle.
func TestForecastLongDropRebuildsAfterLastRead(t *testing.T) {
	store, arch := openLongArchive(t)
	ctx := context.Background()
	if err := arch.ArchiveIssues(ctx, []ForecastArchiveIssue{{Issue: longIssue("kept", time.Now().UTC().Truncate(time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	// A backup still reading on the handle when Sync drops it.
	reading, err := arch.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	store.dropForecastLongArchive(arch)
	if _, err := store.OpenForecastLongArchive(ctx); err == nil {
		t.Fatal("open rebuilt the file while a read on the dropped handle was running")
	}
	if files := quarantinedFiles(t, filepath.Dir(arch.path)); len(files) != 0 {
		t.Fatalf("file moved aside under a running read: %v", files)
	}
	if err := reading.Close(); err != nil {
		t.Fatal(err)
	}
	healed, err := store.OpenForecastLongArchive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if files := quarantinedFiles(t, filepath.Dir(arch.path)); len(files) == 0 {
		t.Fatal("file that passed quick_check was not moved aside after a corruption code")
	}
	if events := store.HealEvents(); len(events) != 1 || events[0].Tier != "forecast_archive" {
		t.Fatalf("heal events = %+v", events)
	}
	if _, err := healed.LoadIssue(ctx, "kept"); err == nil {
		t.Fatal("rebuilt archive still holds the old run")
	}
	again, err := store.OpenForecastLongArchive(ctx)
	if err != nil || again != healed {
		t.Fatalf("second open = %v, %v; want the cached handle", again, err)
	}
}
