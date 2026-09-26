package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RetireRawOnOpen makes Open replace a history file that still holds raw
// polls. Core sets it. Tests leave it unset so fixtures stay intact.
var RetireRawOnOpen bool

// Chart detail stays in SQLite: seven days at ten seconds, 90 days at one
// minute, and five years at one hour. Rollups retain sum/count and extremes.
// Raw polls are not part of the record.
const (
	HistoryHourResolutionMS int64 = 3_600_000
	plainTenSecondKeep            = 7 * 24 * time.Hour
	plainMinuteKeep               = 90 * 24 * time.Hour
	plainHourKeep                 = 5 * 365 * 24 * time.Hour
)

// Tables copied into a history file that no longer carries raw polls. Import
// cursors and Parquet summary receipts travel with the hours they produced:
// without them a restart repeats an import and adds its samples twice.
var plainHistoryTables = []string{
	"ts_drivers", "ts_metrics", "ts_latest",
	"energy_daily", "energy_ledger_meta", "energy_assets", "energy_ledger_entries", "energy_ledger_cursors",
	"ts_buckets", "ts_aggregate_hours", "ts_series_hour", "ts_archive_days",
	"history_dashboard", "history_site_energy", "history_site_cursor",
	"history_migrations", "history_sqlite_progress",
}

// replaceHistoryFile publishes the rebuilt history file. Tests replace it.
var replaceHistoryFile = os.Rename

// RetireRawHistory replaces history.db with a copy that omits raw polls and
// the old hot/warm/cold point log. The previous file stays beside it and is
// not opened again. Ledger rows are counted before the swap.
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
	// Boot refuses to start without history.db, so it must exist after every
	// step. A hard link keeps the old file under both names until one rename
	// replaces it. A filesystem without hard links moves it aside as before.
	linked := os.Link(s.historyPath, retired) == nil
	if !linked {
		if err := os.Rename(s.historyPath, retired); err != nil {
			return err
		}
	}
	var moved []string
	restore := func() {
		for _, suffix := range moved {
			_ = os.Rename(retired+suffix, s.historyPath+suffix)
		}
		if linked {
			_ = os.Remove(retired)
		} else {
			_ = os.Rename(retired, s.historyPath)
		}
	}
	// The old WAL must never be applied to the new file.
	for _, suffix := range []string{"-wal", "-shm"} {
		err := os.Rename(s.historyPath+suffix, retired+suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			restore()
			return err
		}
		moved = append(moved, suffix)
	}
	if err := replaceHistoryFile(freshPath, s.historyPath); err != nil {
		restore()
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Rename(freshPath+suffix, s.historyPath+suffix)
	}
	if err := syncDir(filepath.Dir(s.historyPath)); err != nil {
		return err
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

// AbsorbColdHistory folds sealed bucket files into hourly rows in history.db.
// Import every age: a longer minute policy does not recreate archived rows.
// The files are removed only after every day has been stored. Archive scratch
// goes with them, and so does a sample day whose hourly summary receipt
// matches the file. Other sample days stay readable until the background
// rollup summarizes them. Diagnostics files are left in place.
func (s *Store) AbsorbColdHistory(ctx context.Context, cold string) error {
	if s == nil || s.history == nil || strings.TrimSpace(cold) == "" {
		return nil
	}
	if err := s.lockArchive(ctx); err != nil {
		return err
	}
	defer s.archiveMu.Unlock()
	paths, err := aggregatePaths(cold, 0, time.Now().Add(24*time.Hour).UnixMilli())
	if err != nil {
		return err
	}
	var imported int
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.Contains(path, ".legacy-buckets.") {
			original := strings.Replace(path, ".legacy-buckets.parquet", ".buckets.parquet", 1)
			if _, statErr := os.Stat(original); statErr == nil {
				continue
			}
		}
		digest, err := historyFileHashContext(ctx, path)
		if err != nil {
			return err
		}
		receipt := "plain-archive:" + digest
		hours := map[string]*namedHour{}
		if err := walkBucketFile(ctx, path, func(b metricBucket) error {
			hour := bucketStart(b.StartMS, HistoryHourResolutionMS)
			key := b.Driver + "\x00" + b.Metric + "\x00" + strconv.FormatInt(hour, 10)
			h := hours[key]
			if h == nil {
				h = &namedHour{driver: b.Driver, metric: b.Metric, hour: hour}
				hours[key] = h
			}
			h.sources = append(h.sources, b.BucketSummary)
			return nil
		}); err != nil {
			return fmt.Errorf("read %s: %w", filepath.Base(path), err)
		}
		if err := s.writeHourBuckets(ctx, hours, receipt); err != nil {
			return err
		}
		imported++
	}
	keep, err := s.unsummarizedSampleDays(ctx, cold)
	if err != nil {
		return err
	}
	if err := removeColdHistory(cold, keep); err != nil {
		return err
	}
	removed, err := removeRetiredHistory(s.historyPath)
	if err != nil {
		return err
	}
	if len(keep) > 0 {
		slog.Warn("history sample days kept until they have hourly summaries", "days", len(keep))
	}
	slog.Info("history cold files absorbed", "days", imported, "retired_files", removed)
	return nil
}

// A sample day counts as absorbed only through the summary receipt for this
// exact file. 2.x wrote these days and never summarized them.
func (s *Store) unsummarizedSampleDays(ctx context.Context, cold string) (map[string]bool, error) {
	paths, err := parquetPaths(cold, math.MinInt64, math.MaxInt64)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	for _, path := range paths {
		var receipt string
		err := s.history.QueryRowContext(ctx, `SELECT sha256 FROM ts_archive_days WHERE path=?`, parquetSummaryKey(path)).Scan(&receipt)
		if errors.Is(err, sql.ErrNoRows) {
			keep[path] = true
			continue
		}
		if err != nil {
			return nil, err
		}
		digest, err := historyFileHashContext(ctx, path)
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			slog.Warn("history sample day unreadable; kept", "path", path, "err", err)
		}
		if err != nil || digest != receipt {
			keep[path] = true
		}
	}
	return keep, nil
}

type namedHour struct {
	driver, metric string
	hour           int64
	sources        []BucketSummary
}

func (s *Store) writeHourBuckets(ctx context.Context, hours map[string]*namedHour, receipt string) error {
	if len(hours) == 0 {
		return nil
	}
	ids := map[string][2]int64{}
	for _, h := range hours {
		key := h.driver + "\x00" + h.metric
		if _, ok := ids[key]; ok {
			continue
		}
		d, err := s.driverID(h.driver)
		if err != nil {
			return err
		}
		m, err := s.metricID(h.metric, "")
		if err != nil {
			return err
		}
		ids[key] = [2]int64{d, m}
	}
	// Startup runs this before collection. Each file's receipt, hourly rows
	// and source removal commit together, including a retry after file cleanup
	// failed. Later samples then remain separate contributions to the hour.
	if err := lockContext(ctx, s.archiveViewMu.TryLock); err != nil {
		return err
	}
	defer s.archiveViewMu.Unlock()
	if err := lockContext(ctx, s.historyWriteMu.TryLock); err != nil {
		return err
	}
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var imported bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM history_migrations WHERE name=?)`, receipt).Scan(&imported); err != nil {
		return err
	}
	if imported {
		return nil
	}
	for _, h := range hours {
		id := ids[h.driver+"\x00"+h.metric]
		b, err := reconcileColdHour(ctx, tx, id, h)
		if err != nil {
			return fmt.Errorf("absorb %s/%s at %d: %w", h.driver, h.metric, h.hour, err)
		}
		var existing BucketSummary
		err = tx.QueryRowContext(ctx, `SELECT first_ms,last_ms,n,sum_value,min_value,max_value,last_value FROM ts_buckets
 WHERE driver_id=? AND metric_id=? AND start_ms=? AND resolution_ms=?`, id[0], id[1], h.hour, HistoryHourResolutionMS).
			Scan(&existing.FirstMS, &existing.LastMS, &existing.N, &existing.Sum, &existing.Min, &existing.Max, &existing.Last)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && !sameColdSummary(existing, b) {
			return fmt.Errorf("archive differs from stored hour for %s/%s at %d", h.driver, h.metric, h.hour)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ts_buckets(driver_id,metric_id,start_ms,resolution_ms,first_ms,last_ms,n,sum_value,min_value,max_value,last_value,seen_ms)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,X'')
			ON CONFLICT(driver_id,metric_id,start_ms,resolution_ms) DO NOTHING`,
			id[0], id[1], h.hour, HistoryHourResolutionMS, b.FirstMS, b.LastMS, b.N, b.Sum, b.Min, b.Max, b.Last); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ts_buckets WHERE driver_id=? AND metric_id=? AND start_ms>=? AND start_ms<? AND resolution_ms IN (?,?)`,
			id[0], id[1], h.hour, h.hour+HistoryHourResolutionMS, HistoryResolutionMS, ArchiveResolutionMS); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES (?)`, receipt); err != nil {
		return err
	}
	return tx.Commit()
}

// SQLite minutes may still hold a published archive's source, plus samples
// accepted before its boundary closed. Prefer that complete source minute.
// A compacted interval with only partial SQLite coverage is ambiguous: keep
// the file and all source rows rather than guessing which samples overlap.
func reconcileColdHour(ctx context.Context, tx *sql.Tx, id [2]int64, h *namedHour) (BucketSummary, error) {
	var out BucketSummary
	rows, err := tx.QueryContext(ctx, `SELECT `+bucketColumns+` FROM ts_buckets WHERE driver_id=? AND metric_id=? AND start_ms>=? AND start_ms<? AND resolution_ms=? ORDER BY start_ms`,
		id[0], id[1], h.hour, h.hour+HistoryHourResolutionMS, ArchiveResolutionMS)
	if err != nil {
		return out, err
	}
	var minutes []BucketSummary
	for rows.Next() {
		var b BucketSummary
		if err := rows.Scan(&b.StartMS, &b.ResolutionMS, &b.FirstMS, &b.LastMS, &b.N, &b.Sum, &b.Min, &b.Max, &b.Last); err != nil {
			rows.Close()
			return out, err
		}
		minutes = append(minutes, b)
		out.merge(b)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	for _, b := range h.sources {
		if b.ResolutionMS != ArchiveResolutionMS && b.ResolutionMS != OldArchiveResolutionMS {
			return out, errors.New("unsupported cold history resolution")
		}
		var overlap BucketSummary
		for _, minute := range minutes {
			if minute.StartMS >= b.StartMS && minute.StartMS < b.StartMS+b.ResolutionMS {
				overlap.merge(minute)
			}
		}
		if overlap.N == 0 {
			out.merge(b)
			continue
		}
		if sameColdSummary(overlap, b) {
			continue
		}
		if b.ResolutionMS == ArchiveResolutionMS && overlap.N > b.N && overlap.FirstMS <= b.FirstMS && overlap.LastMS >= b.LastMS && overlap.Min <= b.Min && overlap.Max >= b.Max {
			continue
		}
		return out, errors.New("archive overlaps different SQLite samples")
	}
	return out, nil
}

func sameColdSummary(a, b BucketSummary) bool {
	return a.N == b.N && a.FirstMS == b.FirstMS && a.LastMS == b.LastMS && a.Min == b.Min && a.Max == b.Max && a.Last == b.Last &&
		math.Abs(a.Sum-b.Sum) <= 1e-12*max(1, math.Abs(a.Sum), math.Abs(b.Sum))
}

func removeColdHistory(cold string, keep map[string]bool) error {
	years, err := filepath.Glob(filepath.Join(cold, "[0-9][0-9][0-9][0-9]"))
	if err != nil {
		return err
	}
	for _, year := range years {
		if err := removeColdExcept(year, keep); err != nil {
			return err
		}
	}
	return nil
}

func removeColdExcept(path string, keep map[string]bool) error {
	held := false
	for kept := range keep {
		if strings.HasPrefix(kept, path+string(filepath.Separator)) {
			held = true
			break
		}
	}
	if !held {
		return os.RemoveAll(path)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		child := filepath.Join(path, entry.Name())
		if keep[child] {
			continue
		}
		if err := removeColdExcept(child, keep); err != nil {
			return err
		}
	}
	return nil
}

func removeRetiredHistory(historyPath string) (int, error) {
	matches, err := filepath.Glob(historyPath + ".raw-retired*")
	if err != nil {
		return 0, err
	}
	for _, path := range matches {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	return len(matches), nil
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
	return s.maintainHistory(ctx, "", 0, now, true)
}

func (s *Store) maintainPlainBuckets(ctx context.Context, now time.Time) error {
	// Retain the whole boundary minute so its fine rows do not hide a
	// complete minute summary after only part of the fine data is deleted.
	cutoff10 := bucketStart(now.Add(-plainTenSecondKeep).UnixMilli(), ArchiveResolutionMS)
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
	cutoffHour := bucketStart(now.Add(-plainHourKeep).UnixMilli(), HistoryHourResolutionMS)
	if err := s.deleteBucketSpan(ctx, HistoryHourResolutionMS, cutoffHour); err != nil {
		return err
	}
	for _, table := range []string{"ts_series_hour", "ts_aggregate_hours"} {
		if err := s.deletePlainSummaryHours(ctx, table, cutoffHour); err != nil {
			return err
		}
	}
	return nil
}

// Each delete yields to live collection between bounded commits. A slow
// card reduces the row budget without discarding already committed work.
func (s *Store) deletePlainRows(ctx context.Context, query string, args ...any) error {
	limit := 2048
	for {
		var deleted int64
		err := s.writeArchiveBatch(ctx, func(ctx context.Context, tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, query, append(args, limit)...)
			if err != nil {
				return err
			}
			deleted, err = res.RowsAffected()
			return err
		})
		if err != nil {
			if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if limit == 1 {
				return fmt.Errorf("history maintenance cannot commit one item within its write budget: %w", err)
			}
			limit = max(1, limit/2)
		} else if deleted < int64(limit) {
			return nil
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (s *Store) deleteBucketSpan(ctx context.Context, resolution, cutoff int64) error {
	return s.deletePlainRows(ctx, `DELETE FROM ts_buckets
 WHERE (driver_id,metric_id,start_ms,resolution_ms) IN (
 SELECT driver_id,metric_id,start_ms,resolution_ms FROM ts_buckets
 WHERE resolution_ms=? AND start_ms<? ORDER BY start_ms LIMIT ?)`, resolution, cutoff)
}

// Seek between series using the existing primary key. A time-only scan of
// these tables would revisit years of retained hours in every delete batch.
func (s *Store) deletePlainSummaryHours(ctx context.Context, table string, cutoff int64) error {
	var driver, metric int64
	for {
		err := s.history.QueryRowContext(ctx, `SELECT driver_id,metric_id FROM `+table+`
 WHERE (driver_id,metric_id)>(?,?) ORDER BY driver_id,metric_id LIMIT 1`, driver, metric).Scan(&driver, &metric)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.deletePlainRows(ctx, `DELETE FROM `+table+`
 WHERE (driver_id,metric_id,hour_ms) IN (
 SELECT driver_id,metric_id,hour_ms FROM `+table+`
 WHERE driver_id=? AND metric_id=? AND hour_ms<? ORDER BY hour_ms LIMIT ?)`, driver, metric, cutoff); err != nil {
			return err
		}
	}
}

// Roll complete series-hours atomically: charts must never see a partly
// pruned hour. Preparation uses a snapshot outside the live writer lock.
func (s *Store) rollMinuteHour(ctx context.Context, hour int64) error {
	end := hour + HistoryHourResolutionMS
	limit := 32
	for {
		var groups map[[2]int64]BucketSummary
		err := s.rollupTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
			groups = make(map[[2]int64]BucketSummary)
			rows, err := tx.QueryContext(ctx, `SELECT driver_id,metric_id,first_ms,last_ms,n,sum_value,min_value,max_value,last_value
 FROM ts_buckets WHERE resolution_ms=? AND start_ms>=? AND start_ms<? AND (driver_id,metric_id) IN (
 SELECT driver_id,metric_id FROM ts_buckets WHERE resolution_ms=? AND start_ms>=? AND start_ms<?
 GROUP BY driver_id,metric_id LIMIT ?) ORDER BY driver_id,metric_id,start_ms`,
				ArchiveResolutionMS, hour, end, ArchiveResolutionMS, hour, end, limit)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var key [2]int64
				var b BucketSummary
				if err := rows.Scan(&key[0], &key[1], &b.FirstMS, &b.LastMS, &b.N, &b.Sum, &b.Min, &b.Max, &b.Last); err != nil {
					return err
				}
				merged := groups[key]
				merged.merge(b)
				groups[key] = merged
			}
			return rows.Err()
		}, func(ctx context.Context, tx *sql.Tx) error {
			for key, b := range groups {
				if _, err := tx.ExecContext(ctx, `INSERT INTO ts_buckets(driver_id,metric_id,start_ms,resolution_ms,first_ms,last_ms,n,sum_value,min_value,max_value,last_value,seen_ms)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,X'') ON CONFLICT(driver_id,metric_id,start_ms,resolution_ms) DO UPDATE SET
 first_ms=MIN(first_ms,excluded.first_ms),last_ms=MAX(last_ms,excluded.last_ms),
 last_value=CASE WHEN excluded.last_ms>=last_ms THEN excluded.last_value ELSE last_value END,
 n=n+excluded.n,sum_value=sum_value+excluded.sum_value,min_value=MIN(min_value,excluded.min_value),max_value=MAX(max_value,excluded.max_value)`,
					key[0], key[1], hour, HistoryHourResolutionMS, b.FirstMS, b.LastMS, b.N, b.Sum, b.Min, b.Max, b.Last); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM ts_buckets WHERE driver_id=? AND metric_id=? AND resolution_ms=? AND start_ms>=? AND start_ms<?`,
					key[0], key[1], ArchiveResolutionMS, hour, end); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if limit == 1 {
				return fmt.Errorf("history maintenance cannot commit one item within its write budget: %w", err)
			}
			limit = max(1, limit/2)
		} else if len(groups) < limit {
			return nil
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}
