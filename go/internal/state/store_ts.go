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
type Sample struct {
	Driver string
	Metric string
	TsMs   int64
	Value  float64
}

// internCache holds the in-memory id↔name maps shared across all writers.
type internCache struct {
	mu       sync.RWMutex
	drivers  map[string]int64
	metrics  map[string]int64
	loaded   bool
}

var ts = &internCache{
	drivers: make(map[string]int64),
	metrics: make(map[string]int64),
}

// hydrate loads the existing id mappings from disk. Idempotent.
func (s *Store) hydrateIntern() error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.loaded { return nil }
	rows, err := s.db.Query(`SELECT id, name FROM ts_drivers`)
	if err != nil { return err }
	for rows.Next() {
		var id int64; var name string
		if err := rows.Scan(&id, &name); err != nil { rows.Close(); return err }
		ts.drivers[name] = id
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT id, name FROM ts_metrics`)
	if err != nil { return err }
	for rows.Next() {
		var id int64; var name string
		if err := rows.Scan(&id, &name); err != nil { rows.Close(); return err }
		ts.metrics[name] = id
	}
	rows.Close()
	ts.loaded = true
	return nil
}

// driverID returns the id for a driver name, allocating one on first use.
// Holds the intern mutex for the lookup; safe for concurrent calls.
func (s *Store) driverID(name string) (int64, error) {
	ts.mu.RLock()
	if id, ok := ts.drivers[name]; ok {
		ts.mu.RUnlock()
		return id, nil
	}
	ts.mu.RUnlock()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if id, ok := ts.drivers[name]; ok { return id, nil }
	res, err := s.db.Exec(`INSERT INTO ts_drivers (name) VALUES (?)`, name)
	if err != nil { return 0, err }
	id, err := res.LastInsertId()
	if err != nil { return 0, err }
	ts.drivers[name] = id
	return id, nil
}

// metricID returns the id for a metric name, allocating one on first use.
func (s *Store) metricID(name string) (int64, error) {
	ts.mu.RLock()
	if id, ok := ts.metrics[name]; ok {
		ts.mu.RUnlock()
		return id, nil
	}
	ts.mu.RUnlock()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if id, ok := ts.metrics[name]; ok { return id, nil }
	res, err := s.db.Exec(`INSERT INTO ts_metrics (name) VALUES (?)`, name)
	if err != nil { return 0, err }
	id, err := res.LastInsertId()
	if err != nil { return 0, err }
	ts.metrics[name] = id
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
	if len(samples) == 0 { return nil }
	if err := s.hydrateIntern(); err != nil { return err }

	type resolved struct {
		dID, mID int64
		ts       int64
		v        float64
	}
	rs := make([]resolved, 0, len(samples))
	for _, sm := range samples {
		dID, err := s.driverID(sm.Driver)
		if err != nil { return fmt.Errorf("driver intern %s: %w", sm.Driver, err) }
		mID, err := s.metricID(sm.Metric)
		if err != nil { return fmt.Errorf("metric intern %s: %w", sm.Metric, err) }
		rs = append(rs, resolved{dID: dID, mID: mID, ts: sm.TsMs, v: sm.Value})
	}

	tx, err := s.db.Begin()
	if err != nil { return err }
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO ts_samples (driver_id, metric_id, ts_ms, value) VALUES (?, ?, ?, ?)`)
	if err != nil { return err }
	defer stmt.Close()
	for _, r := range rs {
		if _, err := stmt.Exec(r.dID, r.mID, r.ts, r.v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadSeries returns one metric's history for one driver in [sinceMs, untilMs].
// Result is sorted ascending by ts_ms. maxPoints=0 means no limit; otherwise
// returned rows are evenly downsampled to at most maxPoints.
func (s *Store) LoadSeries(driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]Sample, error) {
	if err := s.hydrateIntern(); err != nil { return nil, err }
	ts.mu.RLock()
	dID, dOK := ts.drivers[driver]
	mID, mOK := ts.metrics[metric]
	ts.mu.RUnlock()
	if !dOK || !mOK { return nil, nil }

	rows, err := s.db.Query(`SELECT ts_ms, value FROM ts_samples
		WHERE driver_id = ? AND metric_id = ? AND ts_ms BETWEEN ? AND ?
		ORDER BY ts_ms ASC`, dID, mID, sinceMs, untilMs)
	if err != nil { return nil, err }
	defer rows.Close()
	out := make([]Sample, 0, 256)
	for rows.Next() {
		var sm Sample
		sm.Driver, sm.Metric = driver, metric
		if err := rows.Scan(&sm.TsMs, &sm.Value); err != nil { return out, err }
		out = append(out, sm)
	}
	if err := rows.Err(); err != nil { return out, err }
	if maxPoints > 0 && len(out) > maxPoints {
		step := float64(len(out)) / float64(maxPoints)
		ds := make([]Sample, 0, maxPoints)
		for i := 0; i < maxPoints; i++ {
			idx := int(float64(i) * step)
			if idx >= len(out) { idx = len(out) - 1 }
			ds = append(ds, out[idx])
		}
		return ds, nil
	}
	return out, nil
}

// LatestSample returns the most recent value for one (driver, metric).
// Returns sql.ErrNoRows if nothing has been recorded.
func (s *Store) LatestSample(driver, metric string) (Sample, error) {
	if err := s.hydrateIntern(); err != nil { return Sample{}, err }
	ts.mu.RLock()
	dID, dOK := ts.drivers[driver]
	mID, mOK := ts.metrics[metric]
	ts.mu.RUnlock()
	if !dOK || !mOK { return Sample{}, sql.ErrNoRows }
	var sm Sample
	sm.Driver, sm.Metric = driver, metric
	err := s.db.QueryRow(`SELECT ts_ms, value FROM ts_samples
		WHERE driver_id = ? AND metric_id = ? ORDER BY ts_ms DESC LIMIT 1`,
		dID, mID).Scan(&sm.TsMs, &sm.Value)
	if errors.Is(err, sql.ErrNoRows) { return sm, err }
	return sm, err
}

// MetricNames returns all known metric names, sorted alphabetically.
func (s *Store) MetricNames() ([]string, error) {
	if err := s.hydrateIntern(); err != nil { return nil, err }
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]string, 0, len(ts.metrics))
	for n := range ts.metrics { out = append(out, n) }
	return out, nil
}

// DriverNames returns all known driver names, sorted alphabetically.
func (s *Store) DriverNames() ([]string, error) {
	if err := s.hydrateIntern(); err != nil { return nil, err }
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]string, 0, len(ts.drivers))
	for n := range ts.drivers { out = append(out, n) }
	return out, nil
}

// PruneRecent deletes samples older than RecentRetention. Caller is expected
// to have already exported them to Parquet via the rollup goroutine.
func (s *Store) PruneRecent(ctx context.Context) (int64, error) {
	cutoff := time.Now().Add(-RecentRetention).UnixMilli()
	res, err := s.db.ExecContext(ctx, `DELETE FROM ts_samples WHERE ts_ms < ?`, cutoff)
	if err != nil { return 0, err }
	return res.RowsAffected()
}

// SamplesBefore streams every sample with ts_ms < cutoff in batches sorted
// ascending by ts_ms, calling the visitor for each batch. The visitor MUST
// not retain the slice past the call (it is reused).
func (s *Store) SamplesBefore(ctx context.Context, cutoffMs int64, batchSize int, visit func([]Sample) error) error {
	if err := s.hydrateIntern(); err != nil { return err }
	if batchSize <= 0 { batchSize = 10000 }
	// Rebuild reverse maps so we can return strings.
	ts.mu.RLock()
	dRev := make(map[int64]string, len(ts.drivers))
	for n, id := range ts.drivers { dRev[id] = n }
	mRev := make(map[int64]string, len(ts.metrics))
	for n, id := range ts.metrics { mRev[id] = n }
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
		if err != nil { return err }
		batch = batch[:0]
		for rows.Next() {
			var dID, mID, t int64; var v float64
			if err := rows.Scan(&dID, &mID, &t, &v); err != nil { rows.Close(); return err }
			batch = append(batch, Sample{Driver: dRev[dID], Metric: mRev[mID], TsMs: t, Value: v})
			lastTs, lastDriverID, lastMetricID = t, dID, mID
			cursorSet = 1
		}
		rows.Close()
		if len(batch) == 0 { return nil }
		if err := visit(batch); err != nil { return err }
		if len(batch) < batchSize { return nil }
	}
}
