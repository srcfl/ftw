package state

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"time"
)

// forecastLongAfterDeleteBatch counts bounded deletes. Tests assert the
// 64-row batches.
var forecastLongAfterDeleteBatch func(table string, n int)

func (a *ForecastLongArchive) maintain(ctx context.Context, now time.Time) error {
	if err := a.thin(ctx, now); err != nil {
		return err
	}
	if err := a.pruneExpired(ctx, now); err != nil {
		return err
	}
	if err := a.pruneToCap(ctx, now); err != nil {
		return err
	}
	if err := a.collectGarbage(ctx); err != nil {
		return err
	}
	return a.incrementalVacuum(ctx)
}

type forecastLongDay struct {
	key   string
	start int64
	end   int64
	ids   []string
}

func (a *ForecastLongArchive) thin(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-a.fullDetail).UnixMilli()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		day, ok, err := a.nextUnthinnedDay(ctx, cutoff)
		if err != nil || !ok {
			return err
		}
		losers, err := a.thinnedLosers(ctx, day.ids)
		if err != nil {
			return err
		}
		if err := a.deleteRunIDs(ctx, losers); err != nil {
			return err
		}
		if err := a.keepDailyState(ctx, day.ids, losers); err != nil {
			return err
		}
		if err := a.markThinned(ctx, day.ids, losers); err != nil {
			return err
		}
		if err := a.collectGarbage(ctx); err != nil {
			return err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (a *ForecastLongArchive) nextUnthinnedDay(ctx context.Context, cutoff int64) (forecastLongDay, bool, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT run_id, origin_ms, timezone FROM fa_runs
 WHERE thinned=0 AND origin_ms<? ORDER BY origin_ms, run_id`, cutoff)
	if err != nil {
		return forecastLongDay{}, false, err
	}
	defer rows.Close()
	var best forecastLongDay
	found := false
	var bestOrigin int64
	grouped := map[string]*forecastLongDay{}
	for rows.Next() {
		var id, tz sql.NullString
		var origin int64
		if err := rows.Scan(&id, &origin, &tz); err != nil {
			return forecastLongDay{}, false, err
		}
		start, end, key := forecastLongLocalDay(origin, tz.String)
		day := grouped[key]
		if day == nil {
			day = &forecastLongDay{key: key, start: start, end: end}
			grouped[key] = day
		}
		day.ids = append(day.ids, id.String)
		if !found || origin < bestOrigin {
			found = true
			bestOrigin = origin
			best = *day
		}
	}
	if err := rows.Err(); err != nil {
		return forecastLongDay{}, false, err
	}
	if !found {
		return forecastLongDay{}, false, nil
	}
	// best was copied before later ids of the same day were appended.
	best = *grouped[best.key]
	return best, true, nil
}

func forecastLongLocalDay(origin int64, tz string) (start, end int64, key string) {
	loc := time.UTC
	if tz != "" {
		if loaded, err := time.LoadLocation(tz); err == nil {
			loc = loaded
		}
	}
	t := time.UnixMilli(origin).In(loc)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	next := day.AddDate(0, 0, 1)
	return day.UnixMilli(), next.UnixMilli(), day.Format("2006-01-02") + "|" + loc.String()
}

func (a *ForecastLongArchive) thinnedLosers(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	query := `SELECT run_id FROM (
 SELECT run_id, ROW_NUMBER() OVER (
  PARTITION BY origin_ms/3600000
  ORDER BY CASE WHEN origin_ms % 3600000 = 0 THEN 0 ELSE 1 END, issued_at_ms, run_id
 ) AS rn
 FROM fa_runs WHERE run_id IN (` + placeholders(len(ids)) + `)
) WHERE rn > 1`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var losers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		losers = append(losers, id)
	}
	return losers, rows.Err()
}

func (a *ForecastLongArchive) keepDailyState(ctx context.Context, ids, losers []string) error {
	drop := map[string]bool{}
	for _, id := range losers {
		drop[id] = true
	}
	var kept []string
	for _, id := range ids {
		if !drop[id] {
			kept = append(kept, id)
		}
	}
	if len(kept) <= 1 {
		return nil
	}
	query := `SELECT run_id FROM fa_runs WHERE run_id IN (` + placeholders(len(kept)) + `)
 ORDER BY origin_ms, issued_at_ms, run_id LIMIT 1`
	args := make([]any, len(kept))
	for i, id := range kept {
		args[i] = id
	}
	var first string
	if err := a.db.QueryRowContext(ctx, query, args...).Scan(&first); err != nil {
		return err
	}
	var extra []string
	for _, id := range kept {
		if id != first {
			extra = append(extra, id)
		}
	}
	return a.deleteWhereIDs(ctx, "fa_run_states", "run_id", extra)
}

func (a *ForecastLongArchive) markThinned(ctx context.Context, ids, losers []string) error {
	drop := map[string]bool{}
	for _, id := range losers {
		drop[id] = true
	}
	var kept []string
	for _, id := range ids {
		if !drop[id] {
			kept = append(kept, id)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	_, err := a.db.ExecContext(ctx, `UPDATE fa_runs SET thinned=1 WHERE run_id IN (`+placeholders(len(kept))+`)`, stringsToArgs(kept)...)
	return err
}

func (a *ForecastLongArchive) pruneExpired(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-a.retention).UnixMilli()
	protect := now.Add(-a.fullDetail).UnixMilli()
	if err := a.deleteRunsBefore(ctx, cutoff, protect, "issued_at_ms"); err != nil {
		return err
	}
	return a.deleteObservationsBefore(ctx, cutoff, protect)
}

func (a *ForecastLongArchive) pruneToCap(ctx context.Context, now time.Time) error {
	protect := now.Add(-a.fullDetail).UnixMilli()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		live, err := a.liveByteCount(ctx)
		if err != nil {
			return err
		}
		if live <= a.maxBytes {
			a.mu.Lock()
			if a.status.State == "over_cap" {
				a.status.State = ""
			}
			a.mu.Unlock()
			return nil
		}
		day, ok, err := a.oldestDay(ctx, protect)
		if err != nil {
			return err
		}
		if !ok {
			a.mu.Lock()
			a.status.State = "over_cap"
			a.mu.Unlock()
			return nil
		}
		if err := a.deleteRunIDs(ctx, day.ids); err != nil {
			return err
		}
		if err := a.deleteObservationsBetween(ctx, day.start, day.end, protect); err != nil {
			return err
		}
		if err := a.collectGarbage(ctx); err != nil {
			return err
		}
	}
}

func (a *ForecastLongArchive) deleteRunsBefore(ctx context.Context, cutoff, protect int64, column string) error {
	for {
		day, ok, err := a.oldestDay(ctx, protect)
		if err != nil || !ok {
			return err
		}
		var ids []string
		query := `SELECT run_id FROM fa_runs WHERE run_id IN (` + placeholders(len(day.ids)) + `) AND ` + column + `<?`
		args := stringsToArgs(day.ids)
		args = append(args, cutoff)
		rows, err := a.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(ids) == 0 {
			return nil
		}
		if err := a.deleteRunIDs(ctx, ids); err != nil {
			return err
		}
	}
}

func (a *ForecastLongArchive) oldestDay(ctx context.Context, protect int64) (forecastLongDay, bool, error) {
	var origin int64
	var tz sql.NullString
	err := a.db.QueryRowContext(ctx, `SELECT origin_ms, timezone FROM fa_runs WHERE origin_ms<? ORDER BY origin_ms, run_id LIMIT 1`, protect).Scan(&origin, &tz)
	if errorsIsNoRows(err) {
		return forecastLongDay{}, false, nil
	}
	if err != nil {
		return forecastLongDay{}, false, err
	}
	start, end, key := forecastLongLocalDay(origin, tz.String)
	rows, err := a.db.QueryContext(ctx, `SELECT run_id, origin_ms, timezone FROM fa_runs WHERE origin_ms>=? AND origin_ms<? AND origin_ms<?`, start, end, protect)
	if err != nil {
		return forecastLongDay{}, false, err
	}
	defer rows.Close()
	day := forecastLongDay{key: key, start: start, end: end}
	for rows.Next() {
		var id string
		var at int64
		var zone sql.NullString
		if err := rows.Scan(&id, &at, &zone); err != nil {
			return forecastLongDay{}, false, err
		}
		_, _, rowKey := forecastLongLocalDay(at, zone.String)
		if rowKey == key {
			day.ids = append(day.ids, id)
		}
	}
	if len(day.ids) == 0 {
		return forecastLongDay{}, false, rows.Err()
	}
	return day, true, rows.Err()
}

func (a *ForecastLongArchive) deleteRunIDs(ctx context.Context, ids []string) error {
	for len(ids) > 0 {
		n := forecastPruneBatch
		if n > len(ids) {
			n = len(ids)
		}
		chunk := ids[:n]
		ids = ids[n:]
		if err := a.beforeWrite(); err != nil {
			return err
		}
		tx, err := a.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, table := range []string{"fa_series", "fa_run_states", "fa_run_weather", "fa_runs"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE run_id IN (`+placeholders(len(chunk))+`)`, stringsToArgs(chunk)...); err != nil {
				tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if forecastLongAfterDeleteBatch != nil {
			forecastLongAfterDeleteBatch("fa_runs", len(chunk))
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *ForecastLongArchive) deleteWhereIDs(ctx context.Context, table, column string, ids []string) error {
	for len(ids) > 0 {
		n := forecastPruneBatch
		if n > len(ids) {
			n = len(ids)
		}
		chunk := ids[:n]
		ids = ids[n:]
		if _, err := a.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+column+` IN (`+placeholders(len(chunk))+`)`, stringsToArgs(chunk)...); err != nil {
			return err
		}
		if forecastLongAfterDeleteBatch != nil {
			forecastLongAfterDeleteBatch(table, len(chunk))
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (a *ForecastLongArchive) deleteObservationsBefore(ctx context.Context, cutoff, protect int64) error {
	limit := cutoff
	if protect < limit {
		limit = protect
	}
	return a.deleteObservationsBetween(ctx, 0, limit, protect)
}

func (a *ForecastLongArchive) deleteObservationsBetween(ctx context.Context, start, end, protect int64) error {
	if end > protect {
		end = protect
	}
	if end <= start && start != 0 {
		return nil
	}
	for {
		query := `DELETE FROM fa_observations_15m WHERE (start_ms, end_ms, config_version) IN (
 SELECT start_ms, end_ms, config_version FROM fa_observations_15m WHERE end_ms<? AND end_ms>=? LIMIT ?)`
		res, err := a.db.ExecContext(ctx, query, end, start, forecastPruneBatch)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil
		}
		if forecastLongAfterDeleteBatch != nil {
			forecastLongAfterDeleteBatch("fa_observations_15m", int(n))
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (a *ForecastLongArchive) collectGarbage(ctx context.Context) error {
	if err := a.deleteOrphans(ctx, `DELETE FROM fa_weather WHERE (source, available_at_ms, start_ms) IN (
 SELECT source, available_at_ms, start_ms FROM fa_weather w
 WHERE NOT EXISTS (SELECT 1 FROM fa_run_weather r WHERE r.available_at_ms=w.available_at_ms)
 LIMIT ?)`, "fa_weather"); err != nil {
		return err
	}
	return a.deleteOrphans(ctx, `DELETE FROM fa_model_states WHERE rowid IN (
 SELECT rowid FROM fa_model_states WHERE id NOT IN (SELECT state_id FROM fa_run_states) LIMIT ?)`, "fa_model_states")
}

func (a *ForecastLongArchive) deleteOrphans(ctx context.Context, query, table string) error {
	for {
		res, err := a.db.ExecContext(ctx, query, forecastPruneBatch)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil
		}
		if forecastLongAfterDeleteBatch != nil {
			forecastLongAfterDeleteBatch(table, int(n))
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (a *ForecastLongArchive) incrementalVacuum(ctx context.Context) error {
	// WAL holds the freed pages until a checkpoint, and incremental_vacuum
	// can only give back pages that are already on the main freelist.
	if _, err := a.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`); err != nil {
		return err
	}
	deadline := time.Now().Add(forecastLongVacuumBudget)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var free, page int64
		if err := a.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
			return err
		}
		if err := a.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&page); err != nil {
			return err
		}
		if free*page < forecastLongFreeTarget || time.Now().After(deadline) {
			return nil
		}
		if _, err := a.db.ExecContext(ctx, `PRAGMA incremental_vacuum(256)`); err != nil {
			return err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (a *ForecastLongArchive) liveByteCount(ctx context.Context) (int64, error) {
	inv, err := sqliteFileInventory(ctx, a.db, a.path)
	if err != nil {
		return 0, err
	}
	return inv.LiveBytes, nil
}

func (a *ForecastLongArchive) recompute(ctx context.Context, src *Store, now time.Time, paused error) error {
	var runs int64
	var oldest, newest sql.NullInt64
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(origin_ms), MAX(origin_ms) FROM fa_runs`).Scan(&runs, &oldest, &newest); err != nil {
		return err
	}
	inv, err := sqliteFileInventory(ctx, a.db, a.path)
	if err != nil {
		return err
	}
	cursor, err := a.issueCursor(ctx)
	if err != nil {
		return err
	}
	lag, err := src.forecastCopyLag(ctx, cursor, now.UnixMilli())
	if err != nil {
		return err
	}
	p50, p95, gz, err := a.issueSizePercentiles(ctx)
	if err != nil {
		return err
	}
	oldestMS := int64(0)
	if oldest.Valid {
		oldestMS = oldest.Int64
	}
	coverage7 := a.hourCoverage(ctx, now, 7*24*time.Hour, oldestMS)
	coverage30 := a.hourCoverage(ctx, now, ForecastLongFullDetail, oldestMS)
	coverageAll := a.hourCoverage(ctx, now, now.Sub(time.UnixMilli(oldestMS))+time.Hour, oldestMS)
	truth := a.truthCoverage(ctx, now, oldestMS)
	a.mu.Lock()
	lastErr := a.status.LastError
	lastCopy := a.status.LastCopyMS
	state := "ok"
	if a.rebuilt {
		state = "rebuilt"
	}
	if a.status.State == "over_cap" {
		state = "over_cap"
	}
	if paused != nil {
		state = "paused_disk_full"
		lastErr = paused.Error()
	}
	a.status = ForecastLongArchiveStatus{
		State: state, LiveBytes: inv.LiveBytes, FileBytes: inv.FileBytes, WALBytes: inv.WALBytes,
		CapBytes: a.maxBytes, OldestOriginMS: oldestMS, NewestOriginMS: newest.Int64, Runs: runs,
		Coverage7d: coverage7, Coverage30d: coverage30, CoverageAll: coverageAll,
		TruthCoverage30d: truth, CopyLagMS: lag, LastCopyMS: lastCopy, LastError: lastErr,
		IssueBytesP50: p50, IssueBytesP95: p95, IssueGzipBytesP50: gz,
	}
	a.mu.Unlock()
	a.publish()
	return nil
}

func (a *ForecastLongArchive) hourCoverage(ctx context.Context, now time.Time, window time.Duration, oldest int64) float64 {
	if oldest <= 0 {
		return 0
	}
	start := now.Add(-window).UnixMilli()
	if oldest > start {
		start = oldest
	}
	start = time.UnixMilli(start).UTC().Truncate(time.Hour).UnixMilli()
	end := now.UTC().Truncate(time.Hour).UnixMilli()
	if end < start {
		return 0
	}
	span := (end-start)/int64(time.Hour/time.Millisecond) + 1
	if span <= 0 {
		return 0
	}
	var present int64
	hour := int64(time.Hour / time.Millisecond)
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT origin_ms/3600000) FROM fa_runs
 WHERE origin_ms>=? AND origin_ms<?`, start, end+hour).Scan(&present); err != nil {
		return 0
	}
	if present > span {
		present = span
	}
	return float64(present) / float64(span)
}

func (a *ForecastLongArchive) truthCoverage(ctx context.Context, now time.Time, oldest int64) float64 {
	if oldest <= 0 {
		return 0
	}
	end := now.UTC().Truncate(15 * time.Minute).UnixMilli()
	start := now.Add(-ForecastLongFullDetail).UnixMilli()
	if oldest > start {
		start = oldest
	}
	start = time.UnixMilli(start).UTC().Truncate(15 * time.Minute).UnixMilli()
	if end <= start {
		return 0
	}
	span := (end - start) / forecastLongQuarterMS
	if span <= 0 {
		return 0
	}
	var known int64
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT start_ms) FROM fa_observations_15m
 WHERE load_known=1 AND start_ms>=? AND start_ms<?`, start, end).Scan(&known); err != nil {
		return 0
	}
	if known > span {
		known = span
	}
	return float64(known) / float64(span)
}

func (a *ForecastLongArchive) issueSizePercentiles(ctx context.Context) (p50, p95, gzipP50 int64, err error) {
	rows, err := a.db.QueryContext(ctx, `SELECT json_bytes, gzip_bytes FROM fa_runs WHERE json_bytes>0`)
	if err != nil {
		return 0, 0, 0, err
	}
	defer rows.Close()
	var jsonBytes, gzipBytes []int64
	for rows.Next() {
		var j, g int64
		if err := rows.Scan(&j, &g); err != nil {
			return 0, 0, 0, err
		}
		jsonBytes = append(jsonBytes, j)
		gzipBytes = append(gzipBytes, g)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, err
	}
	sort.Slice(jsonBytes, func(i, j int) bool { return jsonBytes[i] < jsonBytes[j] })
	sort.Slice(gzipBytes, func(i, j int) bool { return gzipBytes[i] < gzipBytes[j] })
	return percentile(jsonBytes, 0.50), percentile(jsonBytes, 0.95), percentile(gzipBytes, 0.50), nil
}

func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func stringsToArgs(ids []string) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

func errorsIsNoRows(err error) bool {
	return err == sql.ErrNoRows
}
