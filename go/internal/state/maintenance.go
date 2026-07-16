package state

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// CheckpointWAL runs a truncating WAL checkpoint on both DB files. The
// hourly rolloff's bulk DELETEs generate a burst of WAL that auto-checkpoint
// can fail to reclaim if any reader is mid-query at the time; calling this
// right after the rolloff keeps the -wal file from ratcheting upward on an
// SD card. Best-effort: a busy checkpoint just means a reader was active —
// the next hourly run gets another chance.
func (s *Store) CheckpointWAL() {
	if _, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		slog.Debug("state: WAL checkpoint (state.db) skipped", "err", err)
	}
	if s.cache != nil {
		if _, err := s.cache.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			slog.Debug("state: WAL checkpoint (cache.db) skipped", "err", err)
		}
	}
}

// DiskAvail reports the bytes available to the process on the filesystem
// containing dir. Errors on platforms without statfs support (Windows).
func DiskAvail(dir string) (int64, error) {
	return diskAvail(dir)
}

// PruneColdParquet deletes cold-tier day files older than retentionDays
// (both ts_samples days at <coldDir>/YYYY/MM/DD.parquet and diagnostics days
// under <coldDir>/diagnostics/). retentionDays <= 0 keeps everything.
// Empty month/year directories left behind are removed opportunistically.
func PruneColdParquet(coldDir string, retentionDays int, now time.Time) (removed []string, err error) {
	if retentionDays <= 0 || coldDir == "" {
		return nil, nil
	}
	cutoff := now.UTC().AddDate(0, 0, -retentionDays)

	for _, root := range []string{coldDir, filepath.Join(coldDir, "diagnostics")} {
		matches, err := filepath.Glob(filepath.Join(root,
			"[0-9][0-9][0-9][0-9]", "[0-9][0-9]", "[0-9][0-9].parquet"))
		if err != nil {
			return removed, err
		}
		for _, path := range matches {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				continue
			}
			day, err := time.Parse("2006/01/02.parquet", filepath.ToSlash(rel))
			if err != nil {
				continue
			}
			if !day.Before(cutoff) {
				continue
			}
			if err := os.Remove(path); err != nil {
				return removed, fmt.Errorf("remove %s: %w", path, err)
			}
			removed = append(removed, path)
			// Opportunistic cleanup — Remove fails on non-empty dirs, which
			// is exactly the behavior we want, so errors are ignored.
			monthDir := filepath.Dir(path)
			_ = os.Remove(monthDir)
			_ = os.Remove(filepath.Dir(monthDir))
		}
	}
	return removed, nil
}
