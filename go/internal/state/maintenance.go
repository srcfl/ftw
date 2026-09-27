package state

import (
	"context"
	"log/slog"
	"time"
)

// CheckpointWAL copies committed pages without waiting for readers or taking
// the writer lock while waiting for them. SQLite reuses the WAL after readers
// release their snapshots; truncation is an offline maintenance operation.
func (s *Store) CheckpointWAL() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		slog.Debug("state: WAL checkpoint (state.db) skipped", "err", err)
	}
	if s.cache != nil {
		if _, err := s.cache.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
			slog.Debug("state: WAL checkpoint (cache.db) skipped", "err", err)
		}
	}
}

// DiskAvail reports the bytes available to the process on the filesystem
// containing dir. Errors on platforms without statfs support (Windows).
func DiskAvail(dir string) (int64, error) {
	return diskAvail(dir)
}
