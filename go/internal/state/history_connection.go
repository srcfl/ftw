package state

import (
	"context"
	"database/sql/driver"
	"errors"
	"sync"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// historyConnector keeps database/sql stable while rotating a native instance
// between import files. A physical connection holds a read lease until SQL has
// closed its Rows, Tx, statements and Raw calls and finally closes the connection.
// The pool MUST have MaxIdleConns(0); an idle connection would retain its lease.
// Rotation uses TryLock so waiting for a reader never prevents new live writes.
// https://duckdb.org/docs/current/guides/performance/indexing#indexes-and-memory
// explains why connection closure alone cannot release DuckDB's index buffers.
type historyConnector struct {
	mu     sync.RWMutex
	native *duckdb.Connector
	dsn    string
	closed bool
}

func newHistoryConnector(dsn string) (*historyConnector, error) {
	native, err := duckdb.NewConnector(dsn, nil)
	if err != nil {
		return nil, err
	}
	return &historyConnector{native: native, dsn: dsn}, nil
}

func (c *historyConnector) Driver() driver.Driver { return &duckdb.Driver{} }

func (c *historyConnector) lock(ctx context.Context, exclusive bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if exclusive {
			if c.mu.TryLock() {
				return nil
			}
		} else {
			if c.mu.TryRLock() {
				return nil
			}
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *historyConnector) Connect(ctx context.Context) (driver.Conn, error) {
	for {
		if err := c.lock(ctx, false); err != nil {
			return nil, err
		}
		if c.closed {
			c.mu.RUnlock()
			return nil, errors.New("history database is closed")
		}
		if c.native != nil {
			conn, err := c.native.Connect(ctx)
			if err != nil {
				c.mu.RUnlock()
				return nil, err
			}
			return &historyLeaseConn{Conn: conn.(*duckdb.Conn), release: c.mu.RUnlock}, nil
		}
		c.mu.RUnlock()
		// A failed reopen remains a visible storage error. Future connections may
		// retry opening the same durable file after the underlying problem clears.
		if err := c.lock(ctx, true); err != nil {
			return nil, err
		}
		var err error
		if c.native == nil && !c.closed {
			c.native, err = duckdb.NewConnector(c.dsn, nil)
		}
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
}

func (c *historyConnector) rotate(ctx context.Context) error {
	lockCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := c.lock(lockCtx, true)
	cancel()
	if err != nil {
		return err
	}
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("history database is closed")
	}
	if c.native != nil {
		// Flush while there are no live connections, and retain this instance if
		// checkpoint fails. A rotation never interrupts a reader or transaction.
		conn, err := c.native.Connect(ctx)
		if err != nil {
			return err
		}
		_, err = conn.(driver.ExecerContext).ExecContext(ctx, "CHECKPOINT", nil)
		err = errors.Join(err, conn.Close())
		if err != nil {
			return err
		}
		if err := c.native.Close(); err != nil {
			return err
		}
		c.native = nil
	}
	c.native, err = duckdb.NewConnector(c.dsn, nil)
	return err
}

func (c *historyConnector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.native == nil {
		return nil
	}
	err := c.native.Close()
	c.native = nil
	return err
}

// Embedding preserves the driver's optional context and value interfaces.
// database/sql serializes a physical connection; Close is called once.
type historyLeaseConn struct {
	*duckdb.Conn
	release func()
}

func (c *historyLeaseConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

// Call only within sql.Conn.Raw, while database/sql still holds the lease.
func nativeHistoryConn(raw any) driver.Conn {
	if c, ok := raw.(*historyLeaseConn); ok {
		return c.Conn
	}
	return raw.(driver.Conn)
}

func (s *Store) rotateHistory(ctx context.Context) error {
	if s.historyConnector == nil {
		return nil
	}
	for {
		attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := s.historyConnector.rotate(attempt)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return err
		}
		// A long query postpones import. Its lease remains intact and live writer
		// connections can still proceed while the importer waits for a quiet point.
		if s.historyMigration != nil {
			s.historyMigration.update(func(*HistoryMigrationStatus) {})
		}
	}
}
