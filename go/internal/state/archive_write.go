package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"
)

const archiveWriteTimeout = time.Second

// Archive transactions must yield the writer mutex instead of spending the
// live commit's entire budget in SQLite's busy handler. Only the reserved
// connection uses a zero busy timeout; restore it before returning to the pool.
func (s *Store) writeArchiveBatch(ctx context.Context, write func(context.Context, *sql.Tx) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.HistoryWriterStatus().Pending < historyCommitMaxTicks/2 {
			err := s.tryArchiveBatch(ctx, write)
			if !historyWriteBusy(err) {
				return err
			}
		}
		// No transaction, reader lock or writer mutex survives this pause.
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (s *Store) tryArchiveBatch(parent context.Context, write func(context.Context, *sql.Tx) error) error {
	ctx, cancel := context.WithTimeout(parent, archiveWriteTimeout)
	defer cancel()
	conn, err := s.history.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var busyMS int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyMS); err != nil {
		return err
	}
	defer func() {
		restore, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		if _, err := conn.ExecContext(restore, fmt.Sprintf("PRAGMA busy_timeout=%d", busyMS)); err != nil {
			// Never lend a connection with changed lock behaviour to live work.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if _, err := conn.ExecContext(ctx, `PRAGMA busy_timeout=0`); err != nil {
		return err
	}
	// Readers may postpone archive writes, but must never make an archive
	// operation wait while holding the live writer mutex.
	if err := lockContext(ctx, s.archiveViewMu.TryLock); err != nil {
		return err
	}
	defer s.archiveViewMu.Unlock()
	if err := lockContext(ctx, s.historyWriteMu.TryLock); err != nil {
		return err
	}
	defer s.historyWriteMu.Unlock()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := write(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
