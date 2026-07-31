package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ---- Long-format time-series ("recent" tier) ----
//
// Drivers and metric names are interned to integer IDs, kept in process
// memory so writes don't need a roundtrip per sample. The intern caches
// hydrate from disk on first use.

// RecentRetention bounds the SQLite "recent" tier. Older data lives in
// daily Parquet files under <dataDir>/cold/.
const RecentRetention = 14 * 24 * time.Hour

// Sample is one (driver, metric, ts, value) tuple — the canonical TS row.
// Unit is optional display metadata persisted on the metric (not per row).
type Sample struct {
	Driver string
	Metric string
	TsMs   int64
	Value  float64
	Unit   string
}

// metricEntry is the cached intern row for one metric.
type metricEntry struct {
	id   int64
	unit string
}

// internCache holds the in-memory id↔name maps for one Store.
type internCache struct {
	mu      sync.RWMutex
	drivers map[string]int64
	metrics map[string]metricEntry
	loaded  bool
}

func newInternCache() *internCache {
	return &internCache{
		drivers: make(map[string]int64),
		metrics: make(map[string]metricEntry),
	}
}

// hydrate loads the existing id mappings from disk. Idempotent.
func (s *Store) hydrateIntern() error {
	ts := s.ts
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.loaded {
		return nil
	}
	rows, err := s.db.Query(`SELECT id, name FROM ts_drivers`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		ts.drivers[name] = id
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT id, name, COALESCE(unit, '') FROM ts_metrics`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var name, unit string
		if err := rows.Scan(&id, &name, &unit); err != nil {
			rows.Close()
			return err
		}
		ts.metrics[name] = metricEntry{id: id, unit: unit}
	}
	rows.Close()
	ts.loaded = true
	return nil
}

// driverID returns the id for a driver name, allocating one on first use.
// Holds the intern mutex for the lookup; safe for concurrent calls.
func (s *Store) driverID(name string) (int64, error) {
	ts := s.ts
	ts.mu.RLock()
	if id, ok := ts.drivers[name]; ok {
		ts.mu.RUnlock()
		return id, nil
	}
	ts.mu.RUnlock()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if id, ok := ts.drivers[name]; ok {
		return id, nil
	}
	res, err := s.db.Exec(`INSERT INTO ts_drivers (name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	ts.drivers[name] = id
	return id, nil
}

// metricID returns the id for a metric name, allocating one on first use.
// A non-empty unit is persisted the first time it is seen (the unit column
// used to stay NULL forever — units only lived in the in-memory telemetry
// cache, so the catalog lost its labels on every restart).
func (s *Store) metricID(name, unit string) (int64, error) {
	ts := s.ts
	ts.mu.RLock()
	if m, ok := ts.metrics[name]; ok && (unit == "" || m.unit == unit) {
		ts.mu.RUnlock()
		return m.id, nil
	}
	ts.mu.RUnlock()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if m, ok := ts.metrics[name]; ok {
		if unit != "" && m.unit != unit {
			if _, err := s.db.Exec(`UPDATE ts_metrics SET unit = ? WHERE id = ?`, unit, m.id); err != nil {
				return 0, err
			}
			ts.metrics[name] = metricEntry{id: m.id, unit: unit}
		}
		return m.id, nil
	}
	res, err := s.db.Exec(`INSERT INTO ts_metrics (name, unit) VALUES (?, NULLIF(?, ''))`, name, unit)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	ts.metrics[name] = metricEntry{id: id, unit: unit}
	return id, nil
}

// RecordSamples batches a slice of samples into ts_samples. Best-effort:
// rows that conflict with the (driver, metric, ts) primary key are
// skipped (INSERT OR IGNORE) so re-emitting the same tick is harmless.
//
// Deadlock note: ID interning uses s.db.Exec which would block forever if
// called inside the transaction (single-connection pool). Pre-resolve all
// driver/metric IDs first, then run the tx using only stmt.Exec.
func (s *Store) RecordSamples(samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	if err := s.hydrateIntern(); err != nil {
		return err
	}

	type resolved struct {
		dID, mID int64
		ts       int64
		v        float64
	}
	rs := make([]resolved, 0, len(samples))
	for _, sm := range samples {
		dID, err := s.driverID(sm.Driver)
		if err != nil {
			return fmt.Errorf("driver intern %s: %w", sm.Driver, err)
		}
		mID, err := s.metricID(sm.Metric, sm.Unit)
		if err != nil {
			return fmt.Errorf("metric intern %s: %w", sm.Metric, err)
		}
		rs = append(rs, resolved{dID: dID, mID: mID, ts: sm.TsMs, v: sm.Value})
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO ts_samples (driver_id, metric_id, ts_ms, value) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rs {
		if _, err := stmt.Exec(r.dID, r.mID, r.ts, r.v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordTick persists one control-loop tick — the history snapshot plus the
// flushed metric samples — in a single transaction. The loop used to commit
// these separately, doubling the WAL commit rate (~90k commits/day at a 2 s
// tick) for no isolation benefit. Same deadlock note as RecordSamples:
// intern IDs are pre-resolved before the tx opens.
func (s *Store) RecordTick(p HistoryPoint, samples []Sample) error {
	return s.RecordTickWithEnergy(p, samples, nil)
}

// RecordTickWithEnergy persists legacy history, long-format samples, and the
// versioned energy ledger in one transaction.
func (s *Store) RecordTickWithEnergy(p HistoryPoint, samples []Sample, observations []EnergyObservation) error {
	return s.RecordTickWithOptionalHistory(&p, samples, observations)
}

// RecordTickWithOptionalHistory persists long-format samples and energy
// observations, and writes legacy history only when p is non-nil. All selected
// writes share one transaction.
func (s *Store) RecordTickWithOptionalHistory(p *HistoryPoint, samples []Sample, observations []EnergyObservation) error {
	if err := s.hydrateIntern(); err != nil {
		return err
	}
	type resolved struct {
		dID, mID int64
		ts       int64
		v        float64
	}
	rs := make([]resolved, 0, len(samples))
	for _, sm := range samples {
		dID, err := s.driverID(sm.Driver)
		if err != nil {
			return fmt.Errorf("driver intern %s: %w", sm.Driver, err)
		}
		mID, err := s.metricID(sm.Metric, sm.Unit)
		if err != nil {
			return fmt.Errorf("metric intern %s: %w", sm.Metric, err)
		}
		rs = append(rs, resolved{dID: dID, mID: mID, ts: sm.TsMs, v: sm.Value})
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if p != nil {
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO history_hot (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON,
		); err != nil {
			return err
		}
	}
	if len(rs) > 0 {
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO ts_samples (driver_id, metric_id, ts_ms, value) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rs {
			if _, err := stmt.Exec(r.dID, r.mID, r.ts, r.v); err != nil {
				return err
			}
		}
	}
	if err := recordEnergyObservationsTx(tx, observations); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadSeries returns one metric's history for one driver in [sinceMs, untilMs].
// Result is sorted ascending by ts_ms. maxPoints=0 means every raw sample;
// otherwise samples are bucket-averaged in SQL down to at most maxPoints rows
// (Value = bucket AVG, TsMs = latest sample in the bucket, so the newest
// reading always survives downsampling).
func (s *Store) LoadSeries(driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]Sample, error) {
	if maxPoints > 0 {
		pts, err := s.LoadSeriesBuckets(driver, metric, sinceMs, untilMs, maxPoints)
		if err != nil {
			return nil, err
		}
		out := make([]Sample, len(pts))
		for i, p := range pts {
			out[i] = Sample{Driver: driver, Metric: metric, TsMs: p.TsMs, Value: p.V}
		}
		return out, nil
	}
	if err := s.hydrateIntern(); err != nil {
		return nil, err
	}
	ts := s.ts
	ts.mu.RLock()
	dID, dOK := ts.drivers[driver]
	mEnt, mOK := ts.metrics[metric]
	ts.mu.RUnlock()
	if !dOK || !mOK {
		return nil, nil
	}

	rows, err := s.db.Query(`SELECT ts_ms, value FROM ts_samples
		WHERE driver_id = ? AND metric_id = ? AND ts_ms BETWEEN ? AND ?
		ORDER BY ts_ms ASC`, dID, mEnt.id, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Sample, 0, 256)
	for rows.Next() {
		var sm Sample
		sm.Driver, sm.Metric = driver, metric
		if err := rows.Scan(&sm.TsMs, &sm.Value); err != nil {
			return out, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// SeriesPoint is one downsampling bucket of a metric: the average, the
// envelope (min/max — a short spike must not vanish from a zoomed-out
// chart the way pick-every-Nth-sample downsampling made it), and the
// number of raw samples that contributed.
type SeriesPoint struct {
	TsMs int64   `json:"ts"`
	V    float64 `json:"v"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	N    int64   `json:"n"`
}

// LoadSeriesBucketsOrRaw is LoadSeriesBuckets with maxPoints=0 meaning "every
// raw sample" (as degenerate single-sample buckets: v=min=max, n=1), so API
// handlers can serve both shapes from one code path.
func (s *Store) LoadSeriesBucketsOrRaw(driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]SeriesPoint, error) {
	if maxPoints > 0 {
		return s.LoadSeriesBuckets(driver, metric, sinceMs, untilMs, maxPoints)
	}
	raw, err := s.LoadSeries(driver, metric, sinceMs, untilMs, 0)
	if err != nil {
		return nil, err
	}
	out := make([]SeriesPoint, len(raw))
	for i, sm := range raw {
		out[i] = SeriesPoint{TsMs: sm.TsMs, V: sm.Value, Min: sm.Value, Max: sm.Value, N: 1}
	}
	return out, nil
}

// BucketWidthMs is the downsampling bucket width for a window and point
// budget, ceiled so the bucket count never exceeds maxPoints. Exported so
// callers merging cold (Parquet) samples into the same chart can bucket them
// on identical boundaries (origin = sinceMs).
func BucketWidthMs(sinceMs, untilMs int64, maxPoints int) int64 {
	if maxPoints <= 0 {
		return 1
	}
	w := (untilMs - sinceMs + int64(maxPoints)) / int64(maxPoints)
	if w < 1 {
		w = 1
	}
	return w
}

// LoadSeriesBuckets aggregates one (driver, metric) series into at most
// maxPoints buckets, entirely in SQL — the previous approach shipped every
// raw row to Go and then threw most of them away, which on a 2 s cadence
// meant materializing ~40k rows per queried day. TsMs is the latest raw
// sample in each bucket; buckets with no samples are absent (no gap fill).
func (s *Store) LoadSeriesBuckets(driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]SeriesPoint, error) {
	if maxPoints <= 0 || untilMs < sinceMs {
		return nil, nil
	}
	if err := s.hydrateIntern(); err != nil {
		return nil, err
	}
	ts := s.ts
	ts.mu.RLock()
	dID, dOK := ts.drivers[driver]
	mEnt, mOK := ts.metrics[metric]
	ts.mu.RUnlock()
	if !dOK || !mOK {
		return nil, nil
	}

	bucketMs := BucketWidthMs(sinceMs, untilMs, maxPoints)
	rows, err := s.db.Query(`SELECT MAX(ts_ms), AVG(value), MIN(value), MAX(value), COUNT(*)
		FROM ts_samples
		WHERE driver_id = ? AND metric_id = ? AND ts_ms BETWEEN ? AND ?
		GROUP BY (ts_ms - ?) / ?
		ORDER BY 1 ASC`, dID, mEnt.id, sinceMs, untilMs, sinceMs, bucketMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SeriesPoint, 0, maxPoints)
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.TsMs, &p.V, &p.Min, &p.Max, &p.N); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// LatestSample returns the most recent value for one (driver, metric).
// Returns sql.ErrNoRows if nothing has been recorded.
func (s *Store) LatestSample(driver, metric string) (Sample, error) {
	if err := s.hydrateIntern(); err != nil {
		return Sample{}, err
	}
	ts := s.ts
	ts.mu.RLock()
	dID, dOK := ts.drivers[driver]
	mEnt, mOK := ts.metrics[metric]
	ts.mu.RUnlock()
	if !dOK || !mOK {
		return Sample{}, sql.ErrNoRows
	}
	var sm Sample
	sm.Driver, sm.Metric = driver, metric
	err := s.db.QueryRow(`SELECT ts_ms, value FROM ts_samples
		WHERE driver_id = ? AND metric_id = ? ORDER BY ts_ms DESC LIMIT 1`,
		dID, mEnt.id).Scan(&sm.TsMs, &sm.Value)
	if errors.Is(err, sql.ErrNoRows) {
		return sm, err
	}
	return sm, err
}

// MetricInfo is one catalog entry: a metric name plus its display unit
// ("" when the driver never supplied one).
type MetricInfo struct {
	Name string `json:"name"`
	Unit string `json:"unit,omitempty"`
}

// MetricsCatalog returns every known metric with its persisted unit.
func (s *Store) MetricsCatalog() ([]MetricInfo, error) {
	if err := s.hydrateIntern(); err != nil {
		return nil, err
	}
	ts := s.ts
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]MetricInfo, 0, len(ts.metrics))
	for n, m := range ts.metrics {
		out = append(out, MetricInfo{Name: n, Unit: m.unit})
	}
	return out, nil
}

// MetricNames returns all known metric names, sorted alphabetically.
func (s *Store) MetricNames() ([]string, error) {
	if err := s.hydrateIntern(); err != nil {
		return nil, err
	}
	ts := s.ts
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]string, 0, len(ts.metrics))
	for n := range ts.metrics {
		out = append(out, n)
	}
	return out, nil
}

// DriverNames returns all known driver names, sorted alphabetically.
func (s *Store) DriverNames() ([]string, error) {
	if err := s.hydrateIntern(); err != nil {
		return nil, err
	}
	ts := s.ts
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]string, 0, len(ts.drivers))
	for n := range ts.drivers {
		out = append(out, n)
	}
	return out, nil
}

// PruneRecent deletes samples older than RecentRetention. Caller is expected
// to have already exported them to Parquet via the rollup goroutine.
func (s *Store) PruneRecent(ctx context.Context) (int64, error) {
	cutoff := time.Now().Add(-RecentRetention).UnixMilli()
	res, err := s.db.ExecContext(ctx, `DELETE FROM ts_samples WHERE ts_ms < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SamplesBefore streams every sample with ts_ms < cutoff in batches sorted
// ascending by ts_ms, calling the visitor for each batch. The visitor MUST
// not retain the slice past the call (it is reused).
func (s *Store) SamplesBefore(ctx context.Context, cutoffMs int64, batchSize int, visit func([]Sample) error) error {
	if err := s.hydrateIntern(); err != nil {
		return err
	}
	if batchSize <= 0 {
		batchSize = 10000
	}
	// Rebuild reverse maps so we can return strings.
	ts := s.ts
	ts.mu.RLock()
	dRev := make(map[int64]string, len(ts.drivers))
	for n, id := range ts.drivers {
		dRev[id] = n
	}
	mRev := make(map[int64]string, len(ts.metrics))
	for n, m := range ts.metrics {
		mRev[m.id] = n
	}
	ts.mu.RUnlock()

	var lastTs, lastDriverID, lastMetricID int64
	cursorSet := 0
	batch := make([]Sample, 0, batchSize)
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT driver_id, metric_id, ts_ms, value
			FROM ts_samples
			WHERE ts_ms < ?
			  AND (? = 0 OR ts_ms > ? OR (ts_ms = ? AND (driver_id > ? OR (driver_id = ? AND metric_id > ?))))
			ORDER BY ts_ms ASC, driver_id ASC, metric_id ASC
			LIMIT ?`, cutoffMs, cursorSet, lastTs, lastTs, lastDriverID, lastDriverID, lastMetricID, batchSize)
		if err != nil {
			return err
		}
		batch = batch[:0]
		for rows.Next() {
			var dID, mID, t int64
			var v float64
			if err := rows.Scan(&dID, &mID, &t, &v); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, Sample{Driver: dRev[dID], Metric: mRev[mID], TsMs: t, Value: v})
			lastTs, lastDriverID, lastMetricID = t, dID, mID
			cursorSet = 1
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		if err := visit(batch); err != nil {
			return err
		}
		if len(batch) < batchSize {
			return nil
		}
	}
}
