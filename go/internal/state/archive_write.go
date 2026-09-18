package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"
)

const archiveWriteTimeout = time.Second

// Archive transactions must yield the writer mutex instead of spending the
// live commit's entire budget in SQLite's busy handler. Only the reserved
// connection uses a zero busy timeout; restore it before returning to the pool.
func (s *Store) writeArchiveBatch(ctx context.Context, write func(context.Context, *sql.Tx) error) error {
	return s.archiveTransaction(ctx, nil, write)
}

// Preparation reads a SQLite snapshot without owning either writer lock. A
// live commit makes a later snapshot upgrade return BUSY; retry preparation
// as well as the write so a new sample cannot be lost from an hourly total.
func (s *Store) archiveTransaction(ctx context.Context, prepare, write func(context.Context, *sql.Tx) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.HistoryWriterStatus().Pending < historyCommitMaxTicks/2 {
			err := s.tryArchiveBatch(ctx, prepare, write)
			// A prepared hour stays in its current file job on a transient
			// write/lock deadline. Pruning returns deadlines to split its batch.
			retryPrepared := prepare != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
			if !historyWriteBusy(err) && !retryPrepared {
				return err
			}
		}
		// No transaction, reader lock or writer mutex survives this pause.
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (s *Store) tryArchiveBatch(parent context.Context, prepare, write func(context.Context, *sql.Tx) error) error {
	conn, err := s.history.Conn(parent)
	if err != nil {
		return err
	}
	defer conn.Close()
	var busyMS int
	if err := conn.QueryRowContext(parent, `PRAGMA busy_timeout`).Scan(&busyMS); err != nil {
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
	if _, err := conn.ExecContext(parent, `PRAGMA busy_timeout=0`); err != nil {
		return err
	}
	txCtx, cancelTx := context.WithCancel(parent)
	defer cancelTx()
	tx, err := conn.BeginTx(txCtx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if prepare != nil {
		readCtx, cancel := context.WithTimeout(parent, 30*time.Second)
		err := prepare(readCtx, tx)
		cancel()
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(parent, archiveWriteTimeout)
	defer cancel()
	// Tx.Commit uses the context from BeginTx. Arm its deadline only after
	// preparation, so the same short budget also covers the durable commit.
	stopDeadline := context.AfterFunc(ctx, cancelTx)
	defer stopDeadline()
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
	if err := write(ctx, tx); err != nil {
		return err
	}
	err = tx.Commit()
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
