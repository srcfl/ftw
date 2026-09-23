package state

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"sort"
	"time"

	"modernc.org/sqlite"
)

const (
	seriesHourMs         int64 = 60 * 60 * 1000
	seriesHourMinSpanMs        = 48 * seriesHourMs
	seriesHoursMigration       = "ts-series-hour-v1"
)

func useSeriesHourRollup(sinceMs, untilMs int64) bool {
	if untilMs < sinceMs {
		return false
	}
	return untilMs-sinceMs+1 >= seriesHourMinSpanMs
}

type seriesHourKey struct {
	driverID, metricID, hourMs int64
}

func seriesHourOf(tsMs int64) int64 {
	if tsMs >= 0 {
		return (tsMs / seriesHourMs) * seriesHourMs
	}
	// Floor division for negative timestamps used in tests.
	h := tsMs / seriesHourMs * seriesHourMs
	if tsMs%seriesHourMs != 0 {
		h -= seriesHourMs
	}
	return h
}

func (s *Store) seriesHoursReady() bool {
	if s == nil || s.history == nil {
		return false
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_migrations WHERE name=?`, seriesHoursMigration).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func (s *Store) startSeriesHourBackfill() {
	if s == nil || s.history == nil {
		return
	}
	s.seriesHourMu.Lock()
	if s.seriesHourCancel != nil {
		s.seriesHourMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.seriesHourCancel = cancel
	s.seriesHourWG.Add(1)
	s.seriesHourMu.Unlock()
	go func() {
		defer s.seriesHourWG.Done()
		defer cancel()
		s.runSeriesHourBackfill(ctx, 5*time.Second)
	}()
}

func (s *Store) runSeriesHourBackfill(ctx context.Context, retryDelay time.Duration) {
	for ctx.Err() == nil {
		// SQLite work checkpoints each small batch. Parquet keeps a receipt
		// per complete file, so retain its original two-hour work budget;
		// a short whole-attempt deadline would restart a slow day forever.
		// Individual SQLite reads/writes remain bounded to five seconds.
		attemptCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
		err := s.ensureSeriesHours(attemptCtx)
		cancel()
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		slog.Warn("hourly series rollup will resume; raw history remains available", "err", err)
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// The cursor follows the samples' primary key, skipping empty hours and
// avoiding time-index reads scattered across the whole database. Every
// completed batch saves its cursor in the same transaction as its summaries.
// Live/late writes seed a complete hour before adding samples, so an existing
// summary must never be replaced by a background snapshot.
func (s *Store) ensureSeriesHours(ctx context.Context) error {
	if s.history == nil || s.seriesHoursReady() {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.HistoryWriterStatus().Pending >= historyCommitMaxTicks/2 {
			if err := pauseMaintenance(ctx); err != nil {
				return err
			}
			continue
		}
		done, err := s.backfillSeriesHours(ctx, 64)
		if err != nil {
			return err
		}
		if done {
			break
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
	if err := s.ensureParquetHours(ctx); err != nil {
		return err
	}
	if err := lockContext(ctx, s.historyWriteMu.TryLock); err != nil {
		return err
	}
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES (?) ON CONFLICT DO NOTHING`, seriesHoursMigration); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM history_sqlite_progress WHERE source=?`, seriesHoursMigration); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("hourly series rollup ready")
	return nil
}

func (s *Store) backfillSeriesHours(ctx context.Context, maxHours int) (bool, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var done bool
		done, err = s.backfillSeriesHoursSnapshot(ctx, maxHours)
		if err == nil {
			return done, nil
		}
		var sqliteErr *sqlite.Error
		if !errors.As(err, &sqliteErr) || sqliteErr.Code()&0xff != 5 {
			return false, err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return false, err
		}
	}
	return false, err
}

func (s *Store) backfillSeriesHoursSnapshot(ctx context.Context, maxHours int) (bool, error) {
	txCtx, cancelTx := context.WithTimeout(ctx, 10*time.Second)
	defer cancelTx()
	// Deferred: reading never owns SQLite's writer. A concurrent write makes
	// the later upgrade fail with BUSY_SNAPSHOT, including the cursor write.
	tx, err := s.history.BeginTx(txCtx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	readCtx, cancelRead := context.WithTimeout(txCtx, 5*time.Second)
	defer cancelRead()
	var driver, metric, through, rowsDone int64
	err = tx.QueryRowContext(readCtx, `SELECT rows_done,driver_id,metric_id,ts_ms FROM history_sqlite_progress WHERE source=?`, seriesHoursMigration).Scan(&rowsDone, &driver, &metric, &through)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	type hour struct {
		key seriesHourKey
		acc seriesHourAcc
	}
	hours := make([]hour, 0, maxHours)
	done := false
	started := time.Now()
	for len(hours) < maxHours {
		var nextDriver, nextMetric, nextTs int64
		err := tx.QueryRowContext(readCtx, `SELECT driver_id,metric_id,ts_ms FROM ts_samples
			WHERE (driver_id,metric_id,ts_ms) > (?,?,?) ORDER BY driver_id,metric_id,ts_ms LIMIT 1`, driver, metric, through).Scan(&nextDriver, &nextMetric, &nextTs)
		if errors.Is(err, sql.ErrNoRows) {
			done = true
			break
		}
		if err != nil {
			return false, err
		}
		h := hour{key: seriesHourKey{nextDriver, nextMetric, seriesHourOf(nextTs)}}
		if h.key.hourMs > math.MaxInt64-seriesHourMs {
			return false, errors.New("hourly series timestamp exceeds supported range")
		}
		err = tx.QueryRowContext(readCtx, `SELECT SUM(value),MIN(value),MAX(value),COUNT(*),MAX(ts_ms)
			FROM ts_samples WHERE driver_id=? AND metric_id=? AND ts_ms>=? AND ts_ms<?`,
			nextDriver, nextMetric, h.key.hourMs, h.key.hourMs+seriesHourMs).Scan(&h.acc.sum, &h.acc.min, &h.acc.max, &h.acc.n, &h.acc.last)
		if err != nil {
			return false, err
		}
		hours = append(hours, h)
		driver, metric, through = nextDriver, nextMetric, h.key.hourMs+seriesHourMs-1
		rowsDone += h.acc.n
		// Keep useful progress even on a slow disk. The read deadline still
		// bounds an individual dense hour; background retries resume here.
		if time.Since(started) >= 500*time.Millisecond {
			break
		}
	}
	cancelRead()
	if len(hours) == 0 {
		return done, tx.Commit()
	}
	writeCtx, cancelWrite := context.WithTimeout(txCtx, historyCommitTimeout)
	defer cancelWrite()
	if err := lockContext(writeCtx, s.historyWriteMu.TryLock); err != nil {
		return false, err
	}
	defer s.historyWriteMu.Unlock()
	stmt, err := tx.PrepareContext(writeCtx, `INSERT INTO ts_series_hour
		(driver_id,metric_id,hour_ms,sum_value,min_value,max_value,n,last_ts_ms)
		VALUES (?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	for _, h := range hours {
		if _, err := stmt.ExecContext(writeCtx, h.key.driverID, h.key.metricID, h.key.hourMs, h.acc.sum, h.acc.min, h.acc.max, h.acc.n, h.acc.last); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(writeCtx, `INSERT INTO history_sqlite_progress(source,rows_done,driver_id,metric_id,ts_ms)
		VALUES (?,?,?,?,?) ON CONFLICT(source) DO UPDATE SET rows_done=excluded.rows_done,
		driver_id=excluded.driver_id,metric_id=excluded.metric_id,ts_ms=excluded.ts_ms`,
		seriesHoursMigration, rowsDone, driver, metric, through); err != nil {
		return false, err
	}
	return done, tx.Commit()
}

// Raw history remains usable until all summaries and Parquet sources are ready.
func (s *Store) SeriesHourBackfillStatus() map[string]any {
	if s == nil || s.history == nil {
		return map[string]any{"state": "unavailable"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var rows int64
	var ready bool
	err := s.history.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM history_migrations WHERE name=?),
		COALESCE((SELECT rows_done FROM history_sqlite_progress WHERE source=?),0)`, seriesHoursMigration, seriesHoursMigration).Scan(&ready, &rows)
	if err != nil {
		return map[string]any{"state": "unknown", "ready": false}
	}
	if ready {
		return map[string]any{"state": "complete", "ready": true}
	}
	return map[string]any{"state": "rebuilding", "ready": false, "rows_done": rows}
}

type seriesHourAcc struct {
	n             int64
	sum, min, max float64
	last          int64
}

func addSeriesHourSample(acc map[seriesHourKey]*seriesHourAcc, driverID, metricID, tsMs int64, value float64) {
	k := seriesHourKey{driverID, metricID, seriesHourOf(tsMs)}
	a := acc[k]
	if a == nil {
		acc[k] = &seriesHourAcc{n: 1, sum: value, min: value, max: value, last: tsMs}
		return
	}
	a.n++
	a.sum += value
	if value < a.min {
		a.min = value
	}
	if value > a.max {
		a.max = value
	}
	if tsMs > a.last {
		a.last = tsMs
	}
}

func (s *Store) upsertSeriesHoursTx(ctx context.Context, tx *sql.Tx, acc map[seriesHourKey]*seriesHourAcc) error {
	return upsertSeriesHoursTableTx(ctx, tx, "ts_series_hour", acc)
}

// table is a compile-time constant at each call site.
func upsertSeriesHoursTableTx(ctx context.Context, tx *sql.Tx, table string, acc map[seriesHourKey]*seriesHourAcc) error {
	if len(acc) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO `+table+` (driver_id, metric_id, hour_ms, sum_value, min_value, max_value, n, last_ts_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (driver_id, metric_id, hour_ms) DO UPDATE SET
			sum_value = `+table+`.sum_value + excluded.sum_value,
			min_value = MIN(`+table+`.min_value, excluded.min_value),
			max_value = MAX(`+table+`.max_value, excluded.max_value),
			n = `+table+`.n + excluded.n,
			last_ts_ms = MAX(`+table+`.last_ts_ms, excluded.last_ts_ms)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, a := range acc {
		if _, err := stmt.ExecContext(ctx, k.driverID, k.metricID, k.hourMs, a.sum, a.min, a.max, a.n, a.last); err != nil {
			return err
		}
	}
	return nil
}

// refreshSeriesHoursTx rebuilds hour rows from ts_samples. Used after deletes.
func (s *Store) refreshSeriesHoursTx(ctx context.Context, tx *sql.Tx, hours []seriesHourKey) error {
	if len(hours) == 0 {
		return nil
	}
	for _, h := range hours {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM ts_series_hour WHERE driver_id=? AND metric_id=? AND hour_ms=?`,
			h.driverID, h.metricID, h.hourMs); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ts_series_hour (driver_id, metric_id, hour_ms, sum_value, min_value, max_value, n, last_ts_ms)
			SELECT driver_id, metric_id, (ts_ms / ?) * ?, SUM(value), MIN(value), MAX(value), COUNT(*), MAX(ts_ms)
			FROM ts_samples
			WHERE driver_id=? AND metric_id=? AND ts_ms >= ? AND ts_ms < ?
			GROUP BY 1, 2, 3`,
			seriesHourMs, seriesHourMs, h.driverID, h.metricID, h.hourMs, h.hourMs+seriesHourMs); err != nil {
			return err
		}
	}
	return nil
}

type seriesBucketAcc struct {
	first, resolution int64
	lastValue         *float64
	n                 int64
	sum               float64
	min, max          float64
	last              int64
}

func (a *seriesBucketAcc) add(n int64, sum, min, max float64, last int64) {
	if n <= 0 {
		return
	}
	if a.n == 0 {
		a.min, a.max = min, max
	} else {
		if min < a.min {
			a.min = min
		}
		if max > a.max {
			a.max = max
		}
	}
	a.n += n
	a.sum += sum
	if a.n == n || last > a.last {
		a.last = last
	}
}

func (s *Store) loadSeriesBucketsFromHours(ctx context.Context, dID, mID, sinceMs, untilMs int64, maxPoints int) ([]SeriesPoint, error) {
	bucketMs := BucketWidthMs(sinceMs, untilMs, maxPoints)
	acc := make(map[int64]*seriesBucketAcc)
	add := func(key int64, n int64, sum, min, max float64, last int64) {
		if n <= 0 {
			return
		}
		b := acc[key]
		if b == nil {
			b = &seriesBucketAcc{}
			acc[key] = b
		}
		b.add(n, sum, min, max, last)
	}

	firstFull := seriesHourOf(sinceMs)
	if firstFull < sinceMs {
		firstFull += seriesHourMs
	}
	lastFull := seriesHourOf(untilMs)
	if lastFull+seriesHourMs-1 > untilMs {
		lastFull -= seriesHourMs
	}

	if firstFull <= lastFull {
		rows, err := s.history.QueryContext(ctx, `
			SELECT hour_ms, sum_value, min_value, max_value, n, last_ts_ms
			FROM ts_series_hour
			WHERE driver_id=? AND metric_id=? AND hour_ms>=? AND hour_ms<=?`,
			dID, mID, firstFull, lastFull)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var hourMs, n, last int64
			var sum, min, max float64
			if err := rows.Scan(&hourMs, &sum, &min, &max, &n, &last); err != nil {
				rows.Close()
				return nil, err
			}
			add((hourMs-sinceMs)/bucketMs, n, sum, min, max, last)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}

	loadPartial := func(from, to int64) error {
		if to < from {
			return nil
		}
		s.ts.mu.RLock()
		var driver, metric string
		for name, id := range s.ts.drivers {
			if id == dID {
				driver = name
				break
			}
		}
		for name, info := range s.ts.metrics {
			if info.id == mID {
				metric = name
				break
			}
		}
		s.ts.mu.RUnlock()
		return s.walkMergedSeriesStats(ctx, s.coldDir, driver, metric, from, to, func(b BucketSummary) error {
			if !math.IsNaN(b.Sum) {
				add((b.LastMS-sinceMs)/bucketMs, b.N, b.Sum, b.Min, b.Max, b.LastMS)
			}
			return nil
		})
	}
	if firstFull > lastFull {
		if err := loadPartial(sinceMs, untilMs); err != nil {
			return nil, err
		}
	} else {
		if err := loadPartial(sinceMs, firstFull-1); err != nil {
			return nil, err
		}
		if err := loadPartial(lastFull+seriesHourMs, untilMs); err != nil {
			return nil, err
		}
	}

	keys := make([]int64, 0, len(acc))
	for k := range acc {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]SeriesPoint, 0, len(keys))
	for _, k := range keys {
		b := acc[k]
		if b.n == 0 {
			continue
		}
		out = append(out, SeriesPoint{
			TsMs:         b.last,
			ResolutionMS: seriesHourMs,
			V:            b.sum / float64(b.n),
			Min:          b.min,
			Max:          b.max,
			N:            b.n,
		})
	}
	return out, nil
}

func bucketSeriesPoint(b BucketSummary) SeriesPoint {
	p := SeriesPoint{TsMs: b.LastMS, V: b.Sum / float64(b.N), Min: b.Min, Max: b.Max, N: b.N}
	if b.ResolutionMS > 1 {
		p.ResolutionMS = b.ResolutionMS
		p.FirstMS = b.FirstMS
		v := b.Last
		p.Last = &v
	}
	return p
}
func (a *seriesBucketAcc) addBucket(b BucketSummary) {
	if a.n == 0 || b.FirstMS < a.first {
		a.first = b.FirstMS
	}
	if a.n == 0 || b.LastMS >= a.last {
		v := b.Last
		a.lastValue = &v
	}
	a.resolution = max(a.resolution, b.ResolutionMS)
	a.add(b.N, b.Sum, b.Min, b.Max, b.LastMS)
}
func (a *seriesBucketAcc) point() SeriesPoint {
	p := SeriesPoint{TsMs: a.last, V: a.sum / float64(a.n), Min: a.min, Max: a.max, N: a.n}
	if a.resolution > 1 {
		p.FirstMS = a.first
		p.ResolutionMS = a.resolution
		p.Last = a.lastValue
	}
	return p
}
