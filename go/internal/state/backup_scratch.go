package state

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// HoldBackupScratch takes a shared lock on dir, a folder where a backup keeps
// scratch, until release. The kernel drops the lock when the process exits,
// also when systemd kills it before deferred cleanup runs. A folder that
// cannot be locked leaves the backup running without that protection.
func HoldBackupScratch(dir string) (release func()) {
	f, err := lockBackupScratchDir(dir, false)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errors.ErrUnsupported) {
			slog.Debug("backup scratch folder not locked", "dir", dir, "err", err)
		}
		return func() {}
	}
	return func() { _ = f.Close() }
}

// RemoveStaleBackupScratchInShm removes raw copies that stopped backups left
// in /dev/shm. On the home box one held 1.37 GB of RAM for days, so the next
// backup copied on the SD card and ran out of time.
func RemoveStaleBackupScratchInShm() {
	RemoveStaleBackupScratch("/dev/shm", isShmBackupScratch)
}

// RemoveStaleBackupScratch removes the entries in dir that isScratch accepts
// when it can lock dir exclusively, which means no backup is running there.
// It logs each entry it removes with its size and returns the totals.
func RemoveStaleBackupScratch(dir string, isScratch func(fs.DirEntry) bool) (removed int, bytes int64) {
	lock, lockErr := lockBackupScratchDir(dir, true)
	if lockErr == nil {
		defer lock.Close()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	var stale []string
	for _, entry := range entries {
		if isScratch(entry) {
			stale = append(stale, filepath.Join(dir, entry.Name()))
		}
	}
	if len(stale) == 0 {
		return 0, 0
	}
	if lockErr != nil {
		slog.Info("kept backup scratch; a running backup may own it", "dir", dir, "entries", len(stale), "err", lockErr)
		return 0, 0
	}
	for _, path := range stale {
		size := scratchBytes(path)
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("could not remove scratch from a stopped backup", "path", path, "err", err)
			continue
		}
		slog.Info("removed scratch from a stopped backup", "path", path, "bytes", size)
		removed++
		bytes += size
	}
	return removed, bytes
}

// isShmBackupScratch matches backupScratchFile's ftw-backup-<n>.raw and the
// rollback journal SQLite keeps beside it.
func isShmBackupScratch(entry fs.DirEntry) bool {
	stem, ok := strings.CutSuffix(strings.TrimSuffix(entry.Name(), "-journal"), ".raw")
	return ok && entry.Type().IsRegular() && IsTempName(stem, "ftw-backup-")
}

// IsTempName reports whether name is prefix plus the decimal suffix that
// os.CreateTemp and os.MkdirTemp add.
func IsTempName(name, prefix string) bool {
	suffix, ok := strings.CutPrefix(name, prefix)
	return ok && suffix != "" && strings.Trim(suffix, "0123456789") == ""
}

func scratchBytes(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
