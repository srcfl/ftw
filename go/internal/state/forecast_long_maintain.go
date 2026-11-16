package state

import (
	"context"
	"database/sql"
	"errors"
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
	if _, err := a.pruneToCap(ctx, now); err != nil {
		return err
	}
	if err := a.collectGarbage(ctx); err != nil {
		return err
	}
	return a.incrementalVacuum(ctx)
}

// enforceCap runs on every copy between the daily passes. Under the cap and
// above the disk floor it costs one page count.
func (a *ForecastLongArchive) enforceCap(ctx context.Context, now time.Time) error {
	pruned, err := a.pruneToCap(ctx, now)
	if err != nil || !pruned {
		return err
	}
	return a.incrementalVacuum(ctx)
}

type forecastLongDay struct {
	start int64
	end   int64
	ids   []string
}

const forecastLongHourMS = int64(time.Hour / time.Millisecond)

// thin keeps one run per complete UTC hour before the full-detail cutoff and
// model-state references on one run per complete local day. Hours and days
// count as complete when they end at or before the cutoff's whole hour, so a
// day in a half-hour zone waits for its last UTC hour. A day stays unmarked
// until it is complete, and a run copied late into a thinned hour loses to
// the survivor already there.
func (a *ForecastLongArchive) thin(ctx context.Context, now time.Time) error {
	cut := now.Add(-a.fullDetail).UnixMilli()
	cut -= cut % forecastLongHourMS
	from := int64(math.MinInt64)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		origin, tz, ok, err := a.nextUnthinned(ctx, from, cut)
		if err != nil || !ok {
			return err
		}
		start, end, key := forecastLongLocalDay(origin, tz)
		hoursEnd := (end + forecastLongHourMS - 1) / forecastLongHourMS * forecastLongHourMS
		if hoursEnd > cut {
			hoursEnd = cut
		}
		losers, err := a.hourLosers(ctx, start-start%forecastLongHourMS, hoursEnd)
		if err != nil {
			return err
		}
		if err := a.deleteRunIDs(ctx, losers); err != nil {
			return err
		}
		if end <= cut {
			if err := a.thinDay(ctx, start, end, key); err != nil {
				return err
			}
		}
		if err := a.collectGarbage(ctx); err != nil {
			return err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
		from = end
	}
}

// The thinning queries read fa_runs by origin. nextUnthinned runs once per
// local day, so the partial index fa_runs_unthinned spares it a walk over
// every thinned run before the first one left to thin.
const (
	forecastLongNextUnthinned = `SELECT origin_ms, timezone FROM fa_runs
 WHERE thinned=0 AND origin_ms>=? AND origin_ms<? ORDER BY origin_ms LIMIT 1`
	forecastLongHourLosers = `SELECT run_id FROM (
 SELECT run_id, ROW_NUMBER() OVER (
  PARTITION BY origin_ms/3600000
  ORDER BY thinned DESC, CASE WHEN origin_ms % 3600000 = 0 THEN 0 ELSE 1 END, issued_at_ms, run_id
 ) AS rn
 FROM fa_runs WHERE origin_ms>=? AND origin_ms<?
) WHERE rn > 1`
	forecastLongDayRuns = `SELECT run_id, origin_ms, timezone, thinned,
 EXISTS(SELECT 1 FROM fa_run_states s WHERE s.run_id=fa_runs.run_id)
 FROM fa_runs WHERE origin_ms>=? AND origin_ms<? ORDER BY origin_ms, issued_at_ms, run_id`
)

func (a *ForecastLongArchive) nextUnthinned(ctx context.Context, from, cut int64) (int64, string, bool, error) {
	var origin int64
	var tz sql.NullString
	err := a.db.QueryRowContext(ctx, forecastLongNextUnthinned, from, cut).Scan(&origin, &tz)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return origin, tz.String, true, nil
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

// hourLosers ranks every run of each UTC hour in [start, end), thinned or
// not. A thinned survivor keeps its place; otherwise the run on the hour
// wins, then the earliest issued.
func (a *ForecastLongArchive) hourLosers(ctx context.Context, start, end int64) ([]string, error) {
	rows, err := a.db.QueryContext(ctx, forecastLongHourLosers, start, end)
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

// thinDay keeps model-state references on the first run of a complete local
// day that has any, drops them from the other runs, and marks the day thinned.
func (a *ForecastLongArchive) thinDay(ctx context.Context, start, end int64, key string) error {
	rows, err := a.db.QueryContext(ctx, forecastLongDayRuns, start, end)
	if err != nil {
		return err
	}
	held := false
	var extra, unmarked []string
	for rows.Next() {
		var id string
		var origin int64
		var tz sql.NullString
		var thinned, refs int
		if err := rows.Scan(&id, &origin, &tz, &thinned, &refs); err != nil {
			rows.Close()
			return err
		}
		if _, _, k := forecastLongLocalDay(origin, tz.String); k != key {
			continue
		}
		if refs != 0 {
			if held {
				extra = append(extra, id)
			}
			held = true
		}
		if thinned == 0 {
			unmarked = append(unmarked, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if err := a.deleteRunStates(ctx, extra); err != nil {
		return err
	}
	if len(unmarked) == 0 {
		return nil
	}
	_, err = a.db.ExecContext(ctx, `UPDATE fa_runs SET thinned=1 WHERE run_id IN (`+placeholders(len(unmarked))+`)`, stringsToArgs(unmarked)...)
	return err
}

func (a *ForecastLongArchive) pruneExpired(ctx context.Context, now time.Time) error {
	cutoff := now.Add(-a.retention).UnixMilli()
	protect := now.Add(-a.fullDetail).UnixMilli()
	if err := a.deleteRunsIssuedBefore(ctx, cutoff, protect); err != nil {
		return err
	}
	return a.deleteObservationsBefore(ctx, cutoff, protect)
}

// pruneToCap deletes the oldest days outside the full-detail window while
// live pages exceed the cap. Below the disk floor the target drops by the
// missing free space, so the archive gives that much back. It reports
// whether it deleted a day.
func (a *ForecastLongArchive) pruneToCap(ctx context.Context, now time.Time) (bool, error) {
	protect := now.Add(-a.fullDetail).UnixMilli()
	live, err := a.liveByteCount(ctx)
	if err != nil {
		return false, err
	}
	target := a.maxBytes
	if short := a.freeShortfall(); short > 0 && live-short < target {
		target = live - short
	}
	pruned := false
	for live > target {
		if err := ctx.Err(); err != nil {
			return pruned, err
		}
		day, ok, err := a.oldestDay(ctx, protect)
		if err != nil {
			return pruned, err
		}
		if !ok {
			break
		}
		if err := a.deleteRunIDs(ctx, day.ids); err != nil {
			return pruned, err
		}
		if err := a.deleteObservationsBetween(ctx, day.start, day.end, protect); err != nil {
			return pruned, err
		}
		if err := a.collectGarbage(ctx); err != nil {
			return pruned, err
		}
		pruned = true
		if live, err = a.liveByteCount(ctx); err != nil {
			return pruned, err
		}
	}
	a.mu.Lock()
	if live > a.maxBytes {
		a.status.State = "over_cap"
	} else if a.status.State == "over_cap" {
		a.status.State = ""
	}
	a.mu.Unlock()
	return pruned, nil
}

func (a *ForecastLongArchive) deleteRunsIssuedBefore(ctx context.Context, cutoff, protect int64) error {
	for {
		day, ok, err := a.oldestDay(ctx, protect)
		if err != nil || !ok {
			return err
		}
		var ids []string
		query := `SELECT run_id FROM fa_runs WHERE run_id IN (` + placeholders(len(day.ids)) + `) AND issued_at_ms<?`
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
	if errors.Is(err, sql.ErrNoRows) {
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
	day := forecastLongDay{start: start, end: end}
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

// deleteRunIDs does not check the disk floor: deletes give space back.
func (a *ForecastLongArchive) deleteRunIDs(ctx context.Context, ids []string) error {
	for len(ids) > 0 {
		n := forecastPruneBatch
		if n > len(ids) {
			n = len(ids)
		}
		chunk := ids[:n]
		ids = ids[n:]
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

// deleteRunStates drops the model-state references of runs that keep their
// series. Garbage collection removes the states nothing references.
func (a *ForecastLongArchive) deleteRunStates(ctx context.Context, ids []string) error {
	for len(ids) > 0 {
		n := forecastPruneBatch
		if n > len(ids) {
			n = len(ids)
		}
		chunk := ids[:n]
		ids = ids[n:]
		if _, err := a.db.ExecContext(ctx, `DELETE FROM fa_run_states WHERE run_id IN (`+placeholders(len(chunk))+`)`, stringsToArgs(chunk)...); err != nil {
			return err
		}
		if forecastLongAfterDeleteBatch != nil {
			forecastLongAfterDeleteBatch("fa_run_states", len(chunk))
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

// The orphan deletes probe the reference tables through fa_run_weather_fetch
// and fa_run_states_ref, so each candidate row costs one index lookup rather
// than a scan of every reference. A fetch's weather rows stay while any run
// still links that fetch.
const (
	forecastLongWeatherGC = `DELETE FROM fa_weather WHERE (source, available_at_ms, start_ms) IN (
 SELECT source, available_at_ms, start_ms FROM fa_weather w
 WHERE NOT EXISTS (SELECT 1 FROM fa_run_weather WHERE fa_run_weather.source=w.source AND fa_run_weather.available_at_ms=w.available_at_ms)
 LIMIT ?)`
	forecastLongStateGC = `DELETE FROM fa_model_states WHERE rowid IN (
 SELECT rowid FROM fa_model_states m
 WHERE NOT EXISTS (SELECT 1 FROM fa_run_states WHERE fa_run_states.state_id=m.id)
 LIMIT ?)`
)

func (a *ForecastLongArchive) collectGarbage(ctx context.Context) error {
	if err := a.deleteOrphans(ctx, forecastLongWeatherGC, "fa_weather"); err != nil {
		return err
	}
	return a.deleteOrphans(ctx, forecastLongStateGC, "fa_model_states")
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

// incrementalVacuum moves free pages to the end of the file and cuts them
// off. In WAL mode the cut lands in the database file only when a checkpoint
// copies it, so the pass ends with one.
func (a *ForecastLongArchive) incrementalVacuum(ctx context.Context) error {
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
			_, err := a.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
			return err
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
		return fromForecastSource(err)
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
		lastErr = "copying paused: the data volume is full or below the free-space floor"
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
