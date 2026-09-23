package state

import (
	"context"
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
	var total int64
	for {
		n, err := s.tryPruneForecastRows(ctx, table, selection, args...)
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
	txCtx, cancelTx := context.WithCancel(parent)
	defer cancelTx()
	tx, err := s.db.BeginTx(txCtx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	readCtx, cancelRead := context.WithTimeout(parent, 3*time.Second)
	rows, err := tx.QueryContext(readCtx, selection, args...)
	if err != nil {
		cancelRead()
		return 0, err
	}
	ids := make([]any, 0, forecastPruneBatch)
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
		if len(ids) > forecastPruneBatch {
			err = errors.New("forecast prune exceeds batch limit")
			break
		}
	}
	err = errors.Join(err, rows.Err(), rows.Close())
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
	_, err := s.pruneForecastRows(ctx, table, fmt.Sprintf(`SELECT rowid FROM (
 SELECT rowid,%s AS expires,ROW_NUMBER() OVER (ORDER BY %s) AS n,
 SUM(length(payload)) OVER (ORDER BY %s) AS bytes FROM %s)
 WHERE expires<? OR n>? OR bytes>? LIMIT %d`, expiry, order, order, table, forecastPruneBatch),
		now-ForecastIssueRetention.Milliseconds(), maxRows, maxBytes)
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
