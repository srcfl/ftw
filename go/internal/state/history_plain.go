package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// RetireRawOnOpen makes Open replace a history file that still holds raw
// polls. Core sets it. Tests leave it unset so fixtures stay intact.
var RetireRawOnOpen bool

// Chart detail stays in SQLite. Ten-second buckets cover the latest day,
// minute buckets cover a week, and older charts are one row per hour.
// Raw polls are not part of the record.
const (
	HistoryHourResolutionMS int64 = 3_600_000
	plainTenSecondKeep            = 26 * time.Hour
	plainMinuteKeep               = 7 * 24 * time.Hour
	plainHourKeep                 = 2 * 365 * 24 * time.Hour
)

// Tables copied into a history file that no longer carries raw polls.
var plainHistoryTables = []string{
	"ts_drivers", "ts_metrics", "ts_latest",
	"energy_daily", "energy_ledger_meta", "energy_assets", "energy_ledger_entries", "energy_ledger_cursors",
	"ts_buckets", "ts_aggregate_hours", "ts_series_hour",
	"history_dashboard", "history_site_energy", "history_site_cursor",
	"history_migrations",
}

// RetireRawHistory replaces history.db with a copy that omits raw polls and
// the old hot/warm/cold point log. The previous file is renamed beside it
// and is not opened again. Ledger rows are counted before the swap.
func (s *Store) RetireRawHistory(ctx context.Context) error {
	if s == nil || s.history == nil {
		return nil
	}
	var raw int
	if err := s.history.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM ts_samples)`).Scan(&raw); err != nil {
		return err
	}
	if raw == 0 {
		return nil
	}
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()

	freshPath := s.historyPath + ".plain-next"
	_ = os.Remove(freshPath)
	dest, err := openDurableHistory(freshPath)
	if err != nil {
		return err
	}
	failed := true
	defer func() {
		dest.Close()
		if failed {
			_ = os.Remove(freshPath)
			_ = os.Remove(freshPath + "-wal")
			_ = os.Remove(freshPath + "-shm")
		}
	}()
	if err := ensureHistorySchema(func(q string) error { _, err := dest.ExecContext(ctx, q); return err }); err != nil {
		return err
	}
	if _, err := s.history.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		return err
	}
	quoted := strings.ReplaceAll(s.historyPath, "'", "''")
	if _, err := dest.ExecContext(ctx, `ATTACH DATABASE '`+quoted+`' AS src`); err != nil {
		return err
	}
	defer dest.ExecContext(context.Background(), `DETACH src`)
	for _, table := range plainHistoryTables {
		var exists int
		if err := s.history.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		if _, err := dest.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return err
		}
		if _, err := dest.ExecContext(ctx, `INSERT INTO `+table+` SELECT * FROM src.`+table); err != nil {
			return fmt.Errorf("copy %s: %w", table, err)
		}
	}
	srcN, srcSum, err := ledgerDigest(ctx, s.history)
	if err != nil {
		return err
	}
	dstN, dstSum, err := ledgerDigest(ctx, dest)
	if err != nil {
		return err
	}
	if srcN != dstN || srcSum != dstSum {
		return fmt.Errorf("history rebuild ledger mismatch: source %d/%v copy %d/%v", srcN, srcSum, dstN, dstSum)
	}
	if _, err := dest.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	if err := dest.Close(); err != nil {
		return err
	}
	failed = false

	if err := s.history.Close(); err != nil {
		return err
	}
	s.history = nil
	retired := s.historyPath + ".raw-retired"
	if _, err := os.Stat(retired); err == nil {
		retired = fmt.Sprintf("%s.raw-retired-%d", s.historyPath, time.Now().Unix())
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(s.historyPath+suffix, retired+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(freshPath, s.historyPath); err != nil {
		_ = os.Rename(retired, s.historyPath)
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Rename(freshPath+suffix, s.historyPath+suffix)
	}
	reopened, err := openDurableHistory(s.historyPath)
	if err != nil {
		return err
	}
	s.history = reopened
	info, _ := os.Stat(retired)
	var retiredBytes int64
	if info != nil {
		retiredBytes = info.Size()
	}
	slog.Info("history raw polls retired", "kept_ledger_rows", dstN, "retired_bytes", retiredBytes, "retired", retired)
	return nil
}

func ledgerDigest(ctx context.Context, db *sql.DB) (int64, float64, error) {
	var n int64
	var sum float64
	err := db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(energy_wh),0) FROM energy_ledger_entries`).Scan(&n, &sum)
	return n, sum, err
}

// MaintainPlainHistory keeps chart buckets inside SQLite. It does not read
// or write Parquet, and it does not touch raw sample rows.
func (s *Store) MaintainPlainHistory(ctx context.Context, now time.Time) error {
	if s == nil || s.history == nil {
		return nil
	}
	if s.aggregateHistory.Load() {
		if err := s.maintainDashboard(ctx, now); err != nil {
			return err
		}
	}
	cutoff10 := now.Add(-plainTenSecondKeep).UnixMilli()
	if err := s.deleteBucketSpan(ctx, HistoryResolutionMS, cutoff10); err != nil {
		return err
	}
	cutoffMin := now.Add(-plainMinuteKeep).UnixMilli()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var first sql.NullInt64
		if err := s.history.QueryRowContext(ctx, `SELECT MIN(start_ms) FROM ts_buckets WHERE resolution_ms=? AND start_ms<?`, ArchiveResolutionMS, cutoffMin).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			break
		}
		hour := bucketStart(first.Int64, HistoryHourResolutionMS)
		if hour+HistoryHourResolutionMS > cutoffMin {
			break
		}
		if err := s.rollMinuteHour(ctx, hour); err != nil {
			return err
		}
	}
	return s.deleteBucketSpan(ctx, HistoryHourResolutionMS, now.Add(-plainHourKeep).UnixMilli())
}

func (s *Store) deleteBucketSpan(ctx context.Context, resolution, cutoff int64) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var first sql.NullInt64
		if err := s.history.QueryRowContext(ctx, `SELECT MIN(start_ms) FROM ts_buckets WHERE resolution_ms=? AND start_ms<?`, resolution, cutoff).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return nil
		}
		from := bucketStart(first.Int64, HistoryHourResolutionMS)
		to := from + HistoryHourResolutionMS
		if to > cutoff {
			to = cutoff
		}
		res, err := s.history.ExecContext(ctx, `DELETE FROM ts_buckets WHERE resolution_ms=? AND start_ms>=? AND start_ms<?`, resolution, from, to)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil
		}
	}
}

func (s *Store) rollMinuteHour(ctx context.Context, hour int64) error {
	end := hour + HistoryHourResolutionMS
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ts_buckets WHERE resolution_ms=? AND start_ms=?`, HistoryHourResolutionMS, hour); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ts_buckets(driver_id,metric_id,start_ms,resolution_ms,first_ms,last_ms,n,sum_value,min_value,max_value,last_value,seen_ms)
		SELECT driver_id, metric_id, ?, ?, MIN(first_ms), MAX(last_ms), SUM(n), SUM(sum_value), MIN(min_value), MAX(max_value),
			(SELECT last_value FROM ts_buckets c WHERE c.driver_id=b.driver_id AND c.metric_id=b.metric_id AND c.resolution_ms=? AND c.start_ms>=? AND c.start_ms<? ORDER BY c.last_ms DESC LIMIT 1),
			X''
		FROM ts_buckets b
		WHERE resolution_ms=? AND start_ms>=? AND start_ms<?
		GROUP BY driver_id, metric_id`,
		hour, HistoryHourResolutionMS, ArchiveResolutionMS, hour, end, ArchiveResolutionMS, hour, end); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ts_buckets WHERE resolution_ms=? AND start_ms>=? AND start_ms<?`, ArchiveResolutionMS, hour, end); err != nil {
		return err
	}
	return tx.Commit()
}
