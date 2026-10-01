package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const forecastPruneBatch = 64

// Budget scans run in a read snapshot. Only the selected, bounded deletes
// acquire SQLite's writer lock. A concurrent insert invalidates the snapshot;
// retry the selection so it cannot delete from an outdated budget calculation.
func (s *Store) pruneForecastRows(ctx context.Context, table, selection string, args ...any) (int64, error) {
	return s.pruneForecastBatches(ctx, table, forecastRowIDs(selection, args...))
}

type forecastRowSelector func(context.Context, *sql.Tx) ([]any, error)

func (s *Store) pruneForecastBatches(ctx context.Context, table string, selectRows forecastRowSelector) (int64, error) {
	var total int64
	for {
		n, err := s.tryPruneForecastBatch(ctx, table, selectRows)
		if err != nil && !historyWriteBusy(err) && !(ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)) {
			return total, err
		}
		if err == nil {
			total += n
			if n == 0 {
				return total, nil
			}
		}
		if err := pauseMaintenance(ctx); err != nil {
			return total, err
		}
	}
}

// Table and selection are static SQL from this package, never client input.
func (s *Store) tryPruneForecastRows(parent context.Context, table, selection string, args ...any) (int64, error) {
	return s.tryPruneForecastBatch(parent, table, forecastRowIDs(selection, args...))
}

func forecastRowIDs(selection string, args ...any) forecastRowSelector {
	return func(ctx context.Context, tx *sql.Tx) ([]any, error) {
		rows, err := tx.QueryContext(ctx, selection, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		ids := make([]any, 0, forecastPruneBatch)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
			if len(ids) > forecastPruneBatch {
				return nil, errors.New("forecast prune exceeds batch limit")
			}
		}
		return ids, errors.Join(rows.Err(), rows.Close())
	}
}

func (s *Store) tryPruneForecastBatch(parent context.Context, table string, selectRows forecastRowSelector) (int64, error) {
	txCtx, cancelTx := context.WithCancel(parent)
	defer cancelTx()
	tx, err := s.db.BeginTx(txCtx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	readCtx, cancelRead := context.WithTimeout(parent, 3*time.Second)
	ids, err := selectRows(readCtx, tx)
	cancelRead()
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	writeCtx, cancelWrite := context.WithTimeout(parent, time.Second)
	defer cancelWrite()
	stop := context.AfterFunc(writeCtx, cancelTx)
	defer stop()
	res, err := tx.ExecContext(writeCtx, "DELETE FROM "+table+" WHERE rowid IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")", ids...)
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		if writeCtx.Err() != nil {
			err = errors.Join(err, writeCtx.Err())
		}
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) pruneForecastRecords(ctx context.Context, table, order, expiry string, now int64, maxRows, maxBytes int) error {
	// Most pages only replace scores already within the budget. Check without
	// sorting payloads; reserve the ordered scan for actual eviction.
	var count, size, oldest int64
	if err := s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*),
 COALESCE(SUM(length(payload)),0),COALESCE(MIN(%s),0) FROM %s`, expiry, table)).Scan(&count, &size, &oldest); err != nil {
		return err
	}
	if count == 0 || (count <= int64(maxRows) && size <= int64(maxBytes) && oldest >= now-ForecastIssueRetention.Milliseconds()) {
		return nil
	}
	// Expiry does not need a sorted budget scan. Remove it directly in the
	// same small transactions, then check the row and byte budgets again.
	if oldest < now-ForecastIssueRetention.Milliseconds() {
		if _, err := s.pruneForecastRows(ctx, table, fmt.Sprintf("SELECT rowid FROM %s WHERE %s<? LIMIT %d", table, expiry, forecastPruneBatch), now-ForecastIssueRetention.Milliseconds()); err != nil {
			return err
		}
		return s.pruneForecastRecords(ctx, table, order, expiry, now, maxRows, maxBytes)
	}
	// Sort only keys and lengths, then walk the retained prefix in Go. SQL
	// window queries exhausted the read budget on the ARM64 box before
	// returning even one row. Keep memory bounded to one delete batch.
	selection := func(readCtx context.Context, tx *sql.Tx) ([]any, error) {
		rows, err := tx.QueryContext(readCtx, fmt.Sprintf("SELECT rowid,%s,length(payload) FROM %s ORDER BY %s", expiry, table, order))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		ids := make([]any, 0, forecastPruneBatch)
		var count, size int64
		for rows.Next() {
			var id, expires, bytes int64
			if err := rows.Scan(&id, &expires, &bytes); err != nil {
				return nil, err
			}
			count++
			size += bytes
			if expires < now-ForecastIssueRetention.Milliseconds() || count > int64(maxRows) || size > int64(maxBytes) {
				ids = append(ids, id)
				if len(ids) == forecastPruneBatch {
					break
				}
			}
		}
		return ids, errors.Join(rows.Err(), rows.Close())
	}
	_, err := s.pruneForecastBatches(ctx, table, selection)
	return err
}

func (s *Store) pruneForecastIssues(ctx context.Context, now int64) error {
	if err := s.pruneForecastRecords(ctx, "forecast_issues", "issued_at_ms DESC,id DESC", "issued_at_ms", now, MaxForecastIssues, MaxForecastArchiveBytes); err != nil {
		return err
	}
	for {
		if _, err := s.pruneForecastRows(ctx, "forecast_issue_model_states", `SELECT rowid FROM forecast_issue_model_states
 WHERE issue_id NOT IN (SELECT id FROM forecast_issues) LIMIT 64`); err != nil {
			return err
		}
		if _, err := s.pruneForecastRows(ctx, "forecast_model_states", `SELECT rowid FROM forecast_model_states
 WHERE id NOT IN (SELECT state_id FROM forecast_issue_model_states) LIMIT 64`); err != nil {
			return err
		}
		// Evict one oldest issue at a time, then release its unshared states.
		// The sum and reference scans never run after acquiring the writer lock.
		n, err := s.tryPruneForecastRows(ctx, "forecast_issues", `SELECT rowid FROM forecast_issues
 WHERE (SELECT COALESCE(SUM(length(payload)),0) FROM forecast_model_states)>?
 ORDER BY issued_at_ms,id LIMIT 1`, MaxForecastModelStateBytes)
		if err == nil && n == 0 {
			return nil
		}
		if err != nil && !historyWriteBusy(err) && !(ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)) {
			return err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}
