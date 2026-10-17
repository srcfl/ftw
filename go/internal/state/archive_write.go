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

func (s *Store) tryArchiveBatch(parent context.Context, prepare, write func(context.Context, *sql.Tx) error) (err error) {
	timing := newHistoryWriteTiming("connection")
	conn, err := s.history.Conn(parent)
	if err != nil {
		return timing.failure(err)
	}
	var busyMS int
	var tx *sql.Tx
	var restoreBusy, viewLocked, writerLocked bool
	defer func() {
		// Capture the operation before rollback, busy-timeout restoration and
		// connection close. Those cleanup steps keep their existing order.
		err = timing.failure(err)
		if tx != nil {
			_ = tx.Rollback()
		}
		if restoreBusy {
			restore, done := context.WithTimeout(context.Background(), time.Second)
			if _, err := conn.ExecContext(restore, fmt.Sprintf("PRAGMA busy_timeout=%d", busyMS)); err != nil {
				// Never lend changed lock behaviour to live work.
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
			done()
		}
		// ErrTxDone can precede SQLite's automatic rollback. Close waits
		// for that rollback before another live or archive writer can enter.
		_ = conn.Close()
		if writerLocked {
			s.historyWriteMu.Unlock()
		}
		if viewLocked {
			s.archiveViewMu.Unlock()
		}
	}()
	timing.next("busy_timeout")
	if err := conn.QueryRowContext(parent, `PRAGMA busy_timeout`).Scan(&busyMS); err != nil {
		return err
	}
	restoreBusy = true
	if _, err := conn.ExecContext(parent, `PRAGMA busy_timeout=0`); err != nil {
		return err
	}
	txCtx, cancelTx := context.WithCancel(parent)
	defer cancelTx()
	timing.next("begin")
	tx, err = conn.BeginTx(txCtx, nil)
	if err != nil {
		return err
	}
	if prepare != nil {
		timing.next("prepare")
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
	timing.next("archive_view_lock")
	if err := lockContext(ctx, s.archiveViewMu.TryLock); err != nil {
		return err
	}
	viewLocked = true
	timing.next("writer_lock")
	if err := lockContext(ctx, s.historyWriteMu.TryLock); err != nil {
		return err
	}
	writerLocked = true
	timing.next("write")
	if err := write(ctx, tx); err != nil {
		return err
	}
	timing.next("commit")
	err = tx.Commit()
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
