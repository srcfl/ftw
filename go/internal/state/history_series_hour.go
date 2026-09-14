package state

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"sort"
	"time"
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	s.seriesHourCancel = cancel
	s.seriesHourWG.Add(1)
	s.seriesHourMu.Unlock()
	go func() {
		defer s.seriesHourWG.Done()
		defer cancel()
		if err := s.ensureSeriesHours(ctx); err != nil {
			slog.Error("hourly series rollup paused; long-range charts keep using raw samples", "err", err)
		}
	}()
}

// ensureSeriesHours builds ts_series_hour from ts_samples once. Live writes
// keep the table current afterwards. A completed import must run this before
// year-scale /api/series can use the rollup.
func (s *Store) ensureSeriesHours(ctx context.Context) error {
	if s.history == nil {
		return nil
	}
	if s.seriesHoursReady() {
		return nil
	}
	var minTs, maxTs sql.NullInt64
	if err := s.history.QueryRowContext(ctx, `SELECT MIN(ts_ms), MAX(ts_ms) FROM ts_samples`).Scan(&minTs, &maxTs); err != nil {
		return err
	}
	if minTs.Valid {
		start := seriesHourOf(minTs.Int64)
		end := maxTs.Int64 + 1
		const chunk = seriesHourMs
		for t := start; t < end; t += chunk {
			if err := ctx.Err(); err != nil {
				return err
			}
			tEnd := t + chunk
			if tEnd > end {
				tEnd = end
			}
			if err := s.backfillSeriesHour(ctx, t, tEnd); err != nil {
				return err
			}
			if err := pauseMaintenance(ctx); err != nil {
				return err
			}
		}
	}
	if err := s.ensureParquetHours(ctx); err != nil {
		return err
	}

	s.historyWriteMu.Lock()
	_, err := s.history.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES (?) ON CONFLICT DO NOTHING`, seriesHoursMigration)
	s.historyWriteMu.Unlock()
	if err == nil {
		slog.Info("hourly series rollup ready")
	}
	return err
}

// Read the source before starting a write. INSERT ... SELECT reserves SQLite's
// writer for the entire aggregation, even without holding historyWriteMu.
// A slow SD-card read must leave live telemetry free to commit. A timed-out
// backfill leaves the completion marker unset, so reads still use raw samples.
func (s *Store) backfillSeriesHour(ctx context.Context, from, until int64) error {
	readCtx, cancelRead := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRead()
	rows, err := s.history.QueryContext(readCtx, `
		SELECT driver_id, metric_id, (ts_ms / ?) * ?, SUM(value), MIN(value), MAX(value), COUNT(*), MAX(ts_ms)
		FROM ts_samples WHERE ts_ms >= ? AND ts_ms < ? GROUP BY 1, 2, 3`,
		seriesHourMs, seriesHourMs, from, until)
	if err != nil {
		return err
	}
	type hour struct {
		key seriesHourKey
		acc seriesHourAcc
	}
	var hours []hour
	for rows.Next() {
		if len(hours) >= 4096 {
			rows.Close()
			return errors.New("hourly series backfill exceeds 4096 series per hour")
		}
		var h hour
		if err := rows.Scan(&h.key.driverID, &h.key.metricID, &h.key.hourMs,
			&h.acc.sum, &h.acc.min, &h.acc.max, &h.acc.n, &h.acc.last); err != nil {
			rows.Close()
			return err
		}
		hours = append(hours, h)
	}
	err = errors.Join(rows.Err(), rows.Close())
	cancelRead()
	if err != nil {
		return err
	}
	// Keep existing summaries, including hours updated by live writes while
	// the source query ran. Commit small groups and yield between transactions.
	for len(hours) > 0 {
		n := min(len(hours), 128)
		err := func() error {
			writeCtx, cancel := context.WithTimeout(ctx, historyCommitTimeout)
			defer cancel()
			s.historyWriteMu.Lock()
			defer s.historyWriteMu.Unlock()
			tx, err := s.history.BeginTx(writeCtx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			stmt, err := tx.PrepareContext(writeCtx, `
				INSERT INTO ts_series_hour (driver_id, metric_id, hour_ms, sum_value, min_value, max_value, n, last_ts_ms)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`)
			if err != nil {
				return err
			}
			defer stmt.Close()
			for _, h := range hours[:n] {
				if _, err := stmt.ExecContext(writeCtx, h.key.driverID, h.key.metricID, h.key.hourMs,
					h.acc.sum, h.acc.min, h.acc.max, h.acc.n, h.acc.last); err != nil {
					return err
				}
			}
			return tx.Commit()
		}()
		if err != nil {
			return err
		}
		hours = hours[n:]
		if len(hours) > 0 {
			if err := pauseMaintenance(ctx); err != nil {
				return err
			}
		}
	}
	return nil
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
	if len(acc) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO ts_series_hour (driver_id, metric_id, hour_ms, sum_value, min_value, max_value, n, last_ts_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (driver_id, metric_id, hour_ms) DO UPDATE SET
			sum_value = ts_series_hour.sum_value + excluded.sum_value,
			min_value = MIN(ts_series_hour.min_value, excluded.min_value),
			max_value = MAX(ts_series_hour.max_value, excluded.max_value),
			n = ts_series_hour.n + excluded.n,
			last_ts_ms = MAX(ts_series_hour.last_ts_ms, excluded.last_ts_ms)`)
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
	n        int64
	sum      float64
	min, max float64
	last     int64
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
	if last > a.last {
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
		return s.walkMergedSeries(ctx, s.coldDir, driver, metric, from, to, func(ts int64, v float64) error {
			if !math.IsNaN(v) {
				add((ts-sinceMs)/bucketMs, 1, v, v, v, ts)
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
			TsMs: b.last,
			V:    b.sum / float64(b.n),
			Min:  b.min,
			Max:  b.max,
			N:    b.n,
		})
	}
	return out, nil
}
