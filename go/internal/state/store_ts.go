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

// RecentRetention describes the old SQLite/Parquet boundary for legacy exports.
// Primary DuckDB storage uses the configured full-history retention.
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
//
// Two locks, taken in this order — allocMu first, mu second, never the
// reverse:
//
//   - mu guards the maps. It is a memory-only lock and must never be held
//     across disk I/O. Allocation used to hold it exclusively across the
//     INSERT, so on a slow SD card the first sample of a new metric — sent
//     from inside the five-second control tick — parked every API reader
//     (MetricsCatalog, MetricNames, DriverNames, LoadSeries, LatestSample)
//     until SQLite committed. That is the 2026-07-16 prune hazard, expressed
//     as a lock instead of a channel.
//   - allocMu serializes the disk half: one allocator at a time runs its
//     statement and then publishes the id. Readers never take it, so a stuck
//     write cannot reach them. It also keeps the row and the cached entry in
//     step when two callers relabel a metric's unit at once, and lets
//     hydrate swap in freshly scanned maps without dropping an id allocated
//     meanwhile.
type internCache struct {
	allocMu sync.Mutex

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
//
// The two table scans run with no map lock held; readers see the old (empty)
// maps until the finished result is swapped in. allocMu keeps an allocation
// from publishing an id between the scan and the swap, which would otherwise
// be thrown away.
func (s *Store) hydrateIntern() error {
	ts := s.ts
	if ts.isLoaded() {
		return nil
	}
	ts.allocMu.Lock()
	defer ts.allocMu.Unlock()
	if ts.isLoaded() {
		return nil
	}

	drivers := make(map[string]int64)
	rows, err := s.history.Query(`SELECT id, name FROM ts_drivers`)
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
		drivers[name] = id
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	metrics := make(map[string]metricEntry)
	rows, err = s.history.Query(`SELECT id, name, COALESCE(unit, '') FROM ts_metrics`)
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
		metrics[name] = metricEntry{id: id, unit: unit}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	ts.mu.Lock()
	ts.drivers, ts.metrics, ts.loaded = drivers, metrics, true
	ts.mu.Unlock()
	return nil
}

func (c *internCache) isLoaded() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.loaded
}

// driverID returns the id for a driver name, allocating one on first use.
// Safe for concurrent calls: the cached name answers under a read lock, and
// allocation writes to disk with no map lock held (see internCache).
func (s *Store) driverID(name string) (int64, error) {
	ts := s.ts
	ts.mu.RLock()
	id, ok := ts.drivers[name]
	ts.mu.RUnlock()
	if ok {
		return id, nil
	}

	ts.allocMu.Lock()
	defer ts.allocMu.Unlock()
	ts.mu.RLock()
	id, ok = ts.drivers[name]
	ts.mu.RUnlock()
	if ok {
		return id, nil
	}

	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	// ts_drivers.name is UNIQUE, so a row left by an earlier process (or by
	// a caller that raced us before hydrate finished) resolves to the same
	// id rather than failing the whole sample batch.
	if _, err := s.history.Exec(
		`INSERT INTO ts_drivers (id, name) VALUES (nextval('ts_drivers_next_id'), ?) ON CONFLICT(name) DO NOTHING`, name,
	); err != nil {
		return 0, err
	}
	if err := s.history.QueryRow(`SELECT id FROM ts_drivers WHERE name = ?`, name).Scan(&id); err != nil {
		return 0, err
	}

	ts.mu.Lock()
	ts.drivers[name] = id
	ts.mu.Unlock()
	return id, nil
}

// metricID returns the id for a metric name, allocating one on first use.
// A non-empty unit is persisted the first time it is seen (the unit column
// used to stay NULL forever — units only lived in the in-memory telemetry
// cache, so the catalog lost its labels on every restart), and a later
// non-empty unit relabels the metric.
//
// Same locking contract as driverID: no disk write under the map lock.
func (s *Store) metricID(name, unit string) (int64, error) {
	ts := s.ts
	ts.mu.RLock()
	m, ok := ts.metrics[name]
	ts.mu.RUnlock()
	if ok && (unit == "" || m.unit == unit) {
		return m.id, nil
	}

	ts.allocMu.Lock()
	defer ts.allocMu.Unlock()
	ts.mu.RLock()
	m, ok = ts.metrics[name]
	ts.mu.RUnlock()
	if ok && (unit == "" || m.unit == unit) {
		return m.id, nil
	}

	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	// One statement covers both jobs: allocate the row, or relabel an
	// existing one once the driver supplies a unit. An empty unit never
	// erases a label already stored.
	if _, err := s.history.Exec(`INSERT INTO ts_metrics (id, name, unit) VALUES (nextval('ts_metrics_next_id'), ?, NULLIF(?, ''))
		ON CONFLICT(name) DO UPDATE SET unit = COALESCE(NULLIF(excluded.unit, ''), ts_metrics.unit)`,
		name, unit,
	); err != nil {
		return 0, err
	}
	var id int64
	var stored string
	if err := s.history.QueryRow(
		`SELECT id, COALESCE(unit, '') FROM ts_metrics WHERE name = ?`, name,
	).Scan(&id, &stored); err != nil {
		return 0, err
	}

	ts.mu.Lock()
	ts.metrics[name] = metricEntry{id: id, unit: stored}
	ts.mu.Unlock()
	return id, nil
}

// RecordSamples batches a slice of samples into ts_samples. Best-effort:
// rows that conflict with the (driver, metric, ts) primary key are
// skipped (INSERT OR IGNORE) so re-emitting the same tick is harmless.
//
// Deadlock note: ID interning uses s.history.Exec which would block forever if
// called inside the transaction (single-connection pool). Pre-resolve all
// driver/metric IDs first, then run the tx using only stmt.Exec.
func (s *Store) RecordSamples(samples []Sample) error {
	if err := validateHistorySamples(samples); err != nil {
		return err
	}
	if len(samples) == 0 {
		return nil
	}
	if err := s.hydrateIntern(); err != nil {
		return err
	}

	rs, err := s.resolveSamples(samples)
	if err != nil {
		return err
	}

	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.insertSamplesAndHours(context.Background(), tx, rs); err != nil {
		return err
	}
	return tx.Commit()
}

type resolvedSample struct {
	dID, mID int64
	ts       int64
	v        float64
}

func (s *Store) resolveSamples(samples []Sample) ([]resolvedSample, error) {
	rs := make([]resolvedSample, 0, len(samples))
	for _, sm := range samples {
		dID, err := s.driverID(sm.Driver)
		if err != nil {
			return nil, fmt.Errorf("driver intern %s: %w", sm.Driver, err)
		}
		mID, err := s.metricID(sm.Metric, sm.Unit)
		if err != nil {
			return nil, fmt.Errorf("metric intern %s: %w", sm.Metric, err)
		}
		rs = append(rs, resolvedSample{dID: dID, mID: mID, ts: sm.TsMs, v: canonicalHistoryFloat(sm.Value)})
	}
	return rs, nil
}

func (s *Store) insertSamplesAndHours(ctx context.Context, tx *sql.Tx, rs []resolvedSample) error {
	if len(rs) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO ts_samples (driver_id, metric_id, ts_ms, value) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	hours := make(map[seriesHourKey]*seriesHourAcc, 8)
	for _, r := range rs {
		res, err := stmt.ExecContext(ctx, r.dID, r.mID, r.ts, r.v)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		addSeriesHourSample(hours, r.dID, r.mID, r.ts, r.v)
	}
	return s.upsertSeriesHoursTx(ctx, tx, hours)
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := s.recordHistoryBatch(ctx, "", "", p, samples, observations, 0)
	return err
}

// recordHistoryBatch commits data and its retry receipt together. A receipt
// identifies the payload, not its newest timestamp: corrections may be old.
// Only the serial writer supplies acknowledgedSequence: it has observed that
// commit succeed and will never retry it again. Its current, possibly uncertain
// commit retains its receipt until a later batch succeeds.
func (s *Store) recordHistoryBatch(ctx context.Context, batchID, payloadHash string, p *HistoryPoint, samples []Sample, observations []EnergyObservation, acknowledgedSequence int64) (int64, error) {
	if err := validateHistorySamples(samples); err != nil {
		return 0, err
	}
	if p != nil {
		point, err := normalizeHistoryPoint(*p)
		if err != nil {
			return 0, err
		}
		p = &point
	}
	if err := s.hydrateIntern(); err != nil {
		return 0, err
	}
	rs, err := s.resolveSamples(samples)
	if err != nil {
		return 0, err
	}

	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	seq, skipped, err := s.writeHistoryBatchTx(ctx, tx, batchID, payloadHash, p, rs, observations)
	if err != nil {
		return 0, err
	}
	if !skipped {
		if err := ackHistoryReceipts(ctx, tx, seq, acknowledgedSequence); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

func ackHistoryReceipts(ctx context.Context, tx *sql.Tx, seq, acknowledgedSequence int64) error {
	if acknowledgedSequence <= 0 || seq <= 0 {
		return nil
	}
	if acknowledgedSequence >= seq {
		return errors.New("history acknowledgement must precede the current commit")
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM history_receipts WHERE sequence<=?`, acknowledgedSequence)
	return err
}

func (s *Store) writeHistoryBatchTx(ctx context.Context, tx *sql.Tx, batchID, payloadHash string, p *HistoryPoint, rs []resolvedSample, observations []EnergyObservation) (seq int64, skipped bool, err error) {
	if batchID != "" {
		var previous string
		err := tx.QueryRowContext(ctx, `SELECT payload_hash, sequence FROM history_receipts WHERE batch_id=?`, batchID).Scan(&previous, &seq)
		if err == nil {
			if previous != payloadHash {
				return 0, false, errors.New("history batch ID has a different payload")
			}
			return seq, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	if p != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO history_hot (ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON,
		); err != nil {
			return 0, false, err
		}
	}
	if err := s.insertSamplesAndHours(ctx, tx, rs); err != nil {
		return 0, false, err
	}
	if err := recordEnergyObservationsTx(tx, observations); err != nil {
		return 0, false, err
	}
	if batchID != "" {
		if err := tx.QueryRowContext(ctx, `INSERT INTO history_receipts(batch_id,payload_hash) VALUES (?,?) RETURNING sequence`, batchID, payloadHash).Scan(&seq); err != nil {
			return 0, false, err
		}
	}
	return seq, false, nil
}

type historyBatchCommit struct {
	committed int
	rows      int
	seq       int64
}

func (s *Store) recordHistoryBatches(ctx context.Context, batches []historyBatch, acknowledgedSequence int64) (historyBatchCommit, error) {
	var out historyBatchCommit
	if len(batches) == 0 {
		return out, nil
	}
	type prepared struct {
		b  historyBatch
		p  *HistoryPoint
		rs []resolvedSample
	}
	prep := make([]prepared, 0, len(batches))
	for _, b := range batches {
		if err := validateHistorySamples(b.payload.Samples); err != nil {
			return out, err
		}
		var p *HistoryPoint
		if b.payload.Point != nil {
			point, err := normalizeHistoryPoint(*b.payload.Point)
			if err != nil {
				return out, err
			}
			p = &point
		}
		if err := s.hydrateIntern(); err != nil {
			return out, err
		}
		rs, err := s.resolveSamples(b.payload.Samples)
		if err != nil {
			return out, err
		}
		prep = append(prep, prepared{b: b, p: p, rs: rs})
	}

	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	for _, item := range prep {
		seq, skipped, err := s.writeHistoryBatchTx(ctx, tx, item.b.id, item.b.hash, item.p, item.rs, item.b.payload.Observations)
		if err != nil {
			return out, err
		}
		out.committed++
		out.seq = seq
		if !skipped {
			out.rows += len(item.rs) + len(item.b.payload.Observations)
			if item.p != nil {
				out.rows++
			}
		}
	}
	if err := ackHistoryReceipts(ctx, tx, out.seq, acknowledgedSequence); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return out, nil
}

// LoadSeries returns one metric's history for one driver in [sinceMs, untilMs].
// Result is sorted ascending by ts_ms. maxPoints=0 means every raw sample;
// otherwise samples are bucket-averaged in SQL down to at most maxPoints rows
// (Value = bucket AVG, TsMs = latest sample in the bucket, so the newest
// reading always survives downsampling).
func (s *Store) LoadSeries(driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]Sample, error) {
	return s.LoadSeriesContext(context.Background(), driver, metric, sinceMs, untilMs, maxPoints)
}

func (s *Store) LoadSeriesContext(ctx context.Context, driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxPoints > 0 {
		pts, err := s.LoadSeriesBucketsContext(ctx, driver, metric, sinceMs, untilMs, maxPoints)
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

	rows, err := s.history.QueryContext(ctx, `SELECT ts_ms, value FROM ts_samples
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
	return s.LoadSeriesBucketsOrRawContext(context.Background(), driver, metric, sinceMs, untilMs, maxPoints)
}

func (s *Store) LoadSeriesBucketsOrRawContext(ctx context.Context, driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]SeriesPoint, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxPoints > 0 {
		return s.LoadSeriesBucketsContext(ctx, driver, metric, sinceMs, untilMs, maxPoints)
	}
	raw, err := s.LoadSeriesContext(ctx, driver, metric, sinceMs, untilMs, 0)
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
	return s.LoadSeriesBucketsContext(context.Background(), driver, metric, sinceMs, untilMs, maxPoints)
}

func (s *Store) LoadSeriesBucketsContext(ctx context.Context, driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]SeriesPoint, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

	if s.seriesHoursReady() && useSeriesHourRollup(sinceMs, untilMs) {
		return s.loadSeriesBucketsFromHours(ctx, dID, mEnt.id, sinceMs, untilMs, maxPoints)
	}
	bucketMs := BucketWidthMs(sinceMs, untilMs, maxPoints)
	rows, err := s.history.QueryContext(ctx, `SELECT MAX(ts_ms), AVG(value), MIN(value), MAX(value), COUNT(*)
		FROM ts_samples
		WHERE driver_id = ? AND metric_id = ? AND ts_ms BETWEEN ? AND ?
		GROUP BY (ts_ms - ?) // ?
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
	err := s.history.QueryRow(`SELECT ts_ms, value FROM ts_samples
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

// PruneHistorySamples applies the configured raw-history retention in DuckDB.
// A nonpositive retention keeps all samples. The oldest hour is removed per
// transaction, releasing the writer between batches.
func (s *Store) PruneHistorySamples(ctx context.Context, retentionDays int, now time.Time) error {
	// Import receipts refer to committed source chunks. Do not delete their rows
	// until every source has been verified, including after a failed import.
	if retentionDays <= 0 || !s.HistoryMigrationStatus().HistoryComplete {
		return nil
	}
	cutoff := now.UTC().AddDate(0, 0, -retentionDays)
	cutoff = time.Date(cutoff.Year(), cutoff.Month(), cutoff.Day(), 0, 0, 0, 0, time.UTC)
	for {
		var first sql.NullInt64
		if err := s.history.QueryRowContext(ctx, `SELECT MIN(ts_ms) FROM ts_samples WHERE ts_ms < ?`, cutoff.UnixMilli()).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return nil
		}
		if err := s.deleteSamplesChunked(ctx, first.Int64, min(first.Int64+time.Hour.Milliseconds(), cutoff.UnixMilli())); err != nil {
			return err
		}
	}
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
		rows, err := s.history.QueryContext(ctx, `SELECT driver_id, metric_id, ts_ms, value
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
