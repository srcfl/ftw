package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const (
	HotHistoryFilename = "history-hot.db"
	// liveHotRetention is how long SQLite keeps denormalized ticks for
	// 5m/1h/24h charts. DuckDB holds older rows.
	liveHotRetention = 48 * time.Hour
	// hotSealAge copies ticks into DuckDB this long after they land so the
	// archive lags live writes instead of sharing their commit path.
	hotSealAge     = 15 * time.Minute
	hotSealChunk   = 256
	hotUserVersion = 1
)

func hotHistoryPath(statePath string) string {
	return filepath.Join(filepath.Dir(statePath), HotHistoryFilename)
}

func (s *Store) openHotHistory() error {
	path := hotHistoryPath(s.mainDBPath)
	db, err := openRaw(path)
	if err != nil {
		return fmt.Errorf("open live history: %w", err)
	}
	if _, err := db.Exec(`PRAGMA user_version=` + fmt.Sprint(hotUserVersion)); err != nil {
		db.Close()
		return err
	}
	for _, stmt := range hotHistorySchema {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return fmt.Errorf("live history schema: %w", err)
		}
	}
	s.hot = db
	s.hotPath = path
	return nil
}

var hotHistorySchema = []string{
	`CREATE TABLE IF NOT EXISTS history_hot (
		ts_ms INTEGER PRIMARY KEY NOT NULL,
		grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL,
		json TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS ts_drivers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE
	)`,
	`CREATE TABLE IF NOT EXISTS ts_metrics (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		unit TEXT
	)`,
	`CREATE TABLE IF NOT EXISTS ts_samples (
		driver_id INTEGER NOT NULL,
		metric_id INTEGER NOT NULL,
		ts_ms INTEGER NOT NULL,
		value REAL NOT NULL,
		PRIMARY KEY (driver_id, metric_id, ts_ms)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_hot_samples_ts ON ts_samples(ts_ms)`,
	`CREATE TABLE IF NOT EXISTS hot_ticks (
		id TEXT PRIMARY KEY,
		hash TEXT NOT NULL,
		ts_ms INTEGER NOT NULL,
		payload TEXT NOT NULL,
		sealed INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_hot_ticks_seal ON hot_ticks(sealed, ts_ms)`,
}

func (s *Store) recordHotBatches(ctx context.Context, batches []historyBatch, acknowledgedSequence int64) (historyBatchCommit, error) {
	var out historyBatchCommit
	if len(batches) == 0 {
		return out, nil
	}
	if s.hot == nil {
		return out, errors.New("live history is unavailable")
	}
	s.hotWriteMu.Lock()
	defer s.hotWriteMu.Unlock()
	tx, err := s.hot.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
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
		tsMs := tickTimestamp(p, b.payload.Samples, b.payload.Observations)
		payload, err := json.Marshal(b.payload)
		if err != nil {
			return out, err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO hot_ticks(id, hash, ts_ms, payload, sealed) VALUES (?,?,?,?,0)`,
			b.id, b.hash, tsMs, string(payload))
		if err != nil {
			return out, err
		}
		n, _ := res.RowsAffected()
		out.committed++
		out.seq = tsMs
		if n == 0 {
			continue
		}
		if p != nil {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO history_hot(ts_ms, grid_w, pv_w, bat_w, load_w, bat_soc, json)
				VALUES (?,?,?,?,?,?,?)`, p.TsMs, p.GridW, p.PVW, p.BatW, p.LoadW, p.BatSoC, p.JSON); err != nil {
				return out, err
			}
			out.rows++
		}
		for _, sm := range b.payload.Samples {
			dID, err := internHotDriver(ctx, tx, sm.Driver)
			if err != nil {
				return out, err
			}
			mID, err := internHotMetric(ctx, tx, sm.Metric, sm.Unit)
			if err != nil {
				return out, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ts_samples(driver_id, metric_id, ts_ms, value) VALUES (?,?,?,?)`,
				dID, mID, sm.TsMs, canonicalHistoryFloat(sm.Value)); err != nil {
				return out, err
			}
			out.rows++
		}
		out.rows += len(b.payload.Observations)
	}
	_ = acknowledgedSequence
	if err := tx.Commit(); err != nil {
		return out, err
	}
	if err := s.applyHotLedger(ctx, batches); err != nil {
		slog.Warn("energy ledger write postponed; live tick is in SQLite", "err", err)
	}
	return out, nil
}

func (s *Store) applyHotLedger(ctx context.Context, batches []historyBatch) error {
	if s.history == nil {
		return nil
	}
	var obs []EnergyObservation
	for _, b := range batches {
		obs = append(obs, b.payload.Observations...)
	}
	if len(obs) == 0 {
		return nil
	}
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordEnergyObservationsTx(tx, obs); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) checkpointLiveHistory(ctx context.Context) error {
	if s.hot == nil {
		return nil
	}
	s.hotWriteMu.Lock()
	defer s.hotWriteMu.Unlock()
	_, err := s.hot.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return err
}

func tickTimestamp(p *HistoryPoint, samples []Sample, observations []EnergyObservation) int64 {
	var ts int64
	if p != nil {
		ts = p.TsMs
	}
	for _, sm := range samples {
		if sm.TsMs > ts {
			ts = sm.TsMs
		}
	}
	for _, o := range observations {
		if o.AtMs > ts {
			ts = o.AtMs
		}
	}
	return ts
}

func internHotDriver(ctx context.Context, tx *sql.Tx, name string) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM ts_drivers WHERE name=?`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO ts_drivers(name) VALUES (?)`, name)
	if err != nil {
		return 0, err
	}
	id, err = res.LastInsertId()
	return id, err
}

func internHotMetric(ctx context.Context, tx *sql.Tx, name, unit string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO ts_metrics(name, unit) VALUES (?, NULLIF(?, ''))
		ON CONFLICT(name) DO UPDATE SET unit = COALESCE(NULLIF(excluded.unit, ''), ts_metrics.unit)`, name, unit); err != nil {
		return 0, err
	}
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM ts_metrics WHERE name=?`, name).Scan(&id)
	return id, err
}

func (s *Store) hotEarliestMs(ctx context.Context) (int64, bool, error) {
	if s.hot == nil {
		return 0, false, nil
	}
	var ts sql.NullInt64
	if err := s.hot.QueryRowContext(ctx, `SELECT MIN(ts_ms) FROM history_hot`).Scan(&ts); err != nil {
		return 0, false, err
	}
	if !ts.Valid {
		return 0, false, nil
	}
	return ts.Int64, true, nil
}

func (s *Store) loadHotHistory(ctx context.Context, sinceMs, untilMs int64, maxPoints int) ([]HistoryPoint, error) {
	if s.hot == nil {
		return nil, nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if maxPoints > 0 && untilMs >= sinceMs {
		bucketMs := BucketWidthMs(sinceMs, untilMs, maxPoints)
		rows, err = s.hot.QueryContext(ctx, `
			SELECT a.ts_ms, a.grid_w, a.pv_w, a.bat_w, a.load_w, a.bat_soc, h.json
			FROM (
				SELECT MAX(ts_ms) AS ts_ms,
				       AVG(grid_w) AS grid_w, AVG(pv_w) AS pv_w, AVG(bat_w) AS bat_w,
				       AVG(load_w) AS load_w, AVG(bat_soc) AS bat_soc
				FROM history_hot
				WHERE ts_ms BETWEEN ? AND ?
				GROUP BY (ts_ms - ?) / ?
			) a
			JOIN history_hot h ON h.ts_ms = a.ts_ms
			ORDER BY a.ts_ms ASC`, sinceMs, untilMs, sinceMs, bucketMs)
	} else {
		rows, err = s.hot.QueryContext(ctx, `
			SELECT ts_ms, COALESCE(grid_w,0), COALESCE(pv_w,0), COALESCE(bat_w,0),
			       COALESCE(load_w,0), COALESCE(bat_soc,0), json
			FROM history_hot WHERE ts_ms BETWEEN ? AND ? ORDER BY ts_ms ASC`, sinceMs, untilMs)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]HistoryPoint, 0)
	for rows.Next() {
		var p HistoryPoint
		if err := rows.Scan(&p.TsMs, &p.GridW, &p.PVW, &p.BatW, &p.LoadW, &p.BatSoC, &p.JSON); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) loadHotSeries(ctx context.Context, driver, metric string, sinceMs, untilMs int64, maxPoints int) ([]SeriesPoint, error) {
	if s.hot == nil {
		return nil, nil
	}
	var (
		rows *sql.Rows
		err  error
	)
	if maxPoints > 0 && untilMs >= sinceMs {
		bucketMs := BucketWidthMs(sinceMs, untilMs, maxPoints)
		rows, err = s.hot.QueryContext(ctx, `
			SELECT MAX(s.ts_ms), AVG(s.value), MIN(s.value), MAX(s.value), COUNT(*)
			FROM ts_samples s
			JOIN ts_drivers d ON d.id = s.driver_id
			JOIN ts_metrics m ON m.id = s.metric_id
			WHERE d.name = ? AND m.name = ? AND s.ts_ms BETWEEN ? AND ?
			GROUP BY (s.ts_ms - ?) / ?
			ORDER BY 1 ASC`, driver, metric, sinceMs, untilMs, sinceMs, bucketMs)
	} else {
		rows, err = s.hot.QueryContext(ctx, `
			SELECT s.ts_ms, s.value, s.value, s.value, 1
			FROM ts_samples s
			JOIN ts_drivers d ON d.id = s.driver_id
			JOIN ts_metrics m ON m.id = s.metric_id
			WHERE d.name = ? AND m.name = ? AND s.ts_ms BETWEEN ? AND ?
			ORDER BY s.ts_ms ASC`, driver, metric, sinceMs, untilMs)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SeriesPoint, 0)
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.TsMs, &p.V, &p.Min, &p.Max, &p.N); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) dailyEnergyFromHot(ctx context.Context, sinceMs, untilMs int64) (DayEnergy, error) {
	if s.hot == nil {
		return DayEnergy{}, nil
	}
	const q = `
		WITH lagged AS (
			SELECT ts_ms,
			       COALESCE(grid_w, 0) AS grid_w,
			       COALESCE(pv_w,   0) AS pv_w,
			       COALESCE(bat_w,  0) AS bat_w,
			       COALESCE(load_w, 0) AS load_w,
			       LAG(ts_ms) OVER (ORDER BY ts_ms) AS prev_ts
			FROM history_hot WHERE ts_ms BETWEEN ? AND ?
		)
		SELECT
			COALESCE(SUM((CASE WHEN grid_w > 0 THEN  grid_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN grid_w < 0 THEN -grid_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((-pv_w) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN bat_w > 0 THEN  bat_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM((CASE WHEN bat_w < 0 THEN -bat_w ELSE 0 END) * (ts_ms - prev_ts)) / 3600000.0, 0),
			COALESCE(SUM(load_w * (ts_ms - prev_ts)) / 3600000.0, 0),
			COUNT(*)
		FROM lagged
		WHERE prev_ts IS NOT NULL AND (ts_ms - prev_ts) <= ?
	`
	var d DayEnergy
	err := s.hot.QueryRowContext(ctx, q, sinceMs, untilMs, maxCostIntegrationGap.Milliseconds()).Scan(
		&d.ImportWh, &d.ExportWh, &d.PVWh,
		&d.BatChargedWh, &d.BatDischargedWh, &d.LoadWh,
		&d.Intervals,
	)
	return d, err
}

func addDayEnergy(a, b DayEnergy) DayEnergy {
	a.ImportWh += b.ImportWh
	a.ExportWh += b.ExportWh
	a.PVWh += b.PVWh
	a.BatChargedWh += b.BatChargedWh
	a.BatDischargedWh += b.BatDischargedWh
	a.LoadWh += b.LoadWh
	a.Intervals += b.Intervals
	return a
}

func mergeHistoryPoints(hot, arch []HistoryPoint) []HistoryPoint {
	if len(hot) == 0 {
		return arch
	}
	if len(arch) == 0 {
		return hot
	}
	byTS := make(map[int64]HistoryPoint, len(hot)+len(arch))
	for _, p := range arch {
		byTS[p.TsMs] = p
	}
	for _, p := range hot {
		byTS[p.TsMs] = p
	}
	out := make([]HistoryPoint, 0, len(byTS))
	for _, p := range byTS {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TsMs < out[j].TsMs })
	return out
}

func mergeSeriesPoints(hot, arch []SeriesPoint) []SeriesPoint {
	if len(hot) == 0 {
		return arch
	}
	if len(arch) == 0 {
		return hot
	}
	byTS := make(map[int64]SeriesPoint, len(hot)+len(arch))
	for _, p := range arch {
		byTS[p.TsMs] = p
	}
	for _, p := range hot {
		byTS[p.TsMs] = p
	}
	out := make([]SeriesPoint, 0, len(byTS))
	for _, p := range byTS {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TsMs < out[j].TsMs })
	return out
}

func downsampleHistory(pts []HistoryPoint, sinceMs, untilMs int64, maxPoints int) []HistoryPoint {
	if maxPoints <= 0 || len(pts) <= maxPoints {
		return pts
	}
	bucketMs := BucketWidthMs(sinceMs, untilMs, maxPoints)
	type acc struct {
		n                        int
		ts                       int64
		grid, pv, bat, load, soc float64
		json                     string
	}
	buckets := map[int64]*acc{}
	order := make([]int64, 0)
	for _, p := range pts {
		b := (p.TsMs - sinceMs) / bucketMs
		a := buckets[b]
		if a == nil {
			a = &acc{}
			buckets[b] = a
			order = append(order, b)
		}
		a.n++
		a.grid += p.GridW
		a.pv += p.PVW
		a.bat += p.BatW
		a.load += p.LoadW
		a.soc += p.BatSoC
		if p.TsMs >= a.ts {
			a.ts = p.TsMs
			a.json = p.JSON
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]HistoryPoint, 0, len(order))
	for _, b := range order {
		a := buckets[b]
		n := float64(a.n)
		out = append(out, HistoryPoint{
			TsMs: a.ts, GridW: a.grid / n, PVW: a.pv / n, BatW: a.bat / n,
			LoadW: a.load / n, BatSoC: a.soc / n, JSON: a.json,
		})
	}
	return out
}

func downsampleSeries(pts []SeriesPoint, sinceMs, untilMs int64, maxPoints int) []SeriesPoint {
	if maxPoints <= 0 || len(pts) <= maxPoints {
		return pts
	}
	bucketMs := BucketWidthMs(sinceMs, untilMs, maxPoints)
	type acc struct {
		n, ts         int64
		sum, min, max float64
	}
	buckets := map[int64]*acc{}
	order := make([]int64, 0)
	for _, p := range pts {
		b := (p.TsMs - sinceMs) / bucketMs
		a := buckets[b]
		if a == nil {
			a = &acc{min: p.Min, max: p.Max}
			buckets[b] = a
			order = append(order, b)
		}
		a.n += p.N
		a.sum += p.V * float64(p.N)
		if p.Min < a.min {
			a.min = p.Min
		}
		if p.Max > a.max {
			a.max = p.Max
		}
		if p.TsMs >= a.ts {
			a.ts = p.TsMs
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	out := make([]SeriesPoint, 0, len(order))
	for _, b := range order {
		a := buckets[b]
		v := a.sum
		if a.n > 0 {
			v = a.sum / float64(a.n)
		}
		out = append(out, SeriesPoint{TsMs: a.ts, V: v, Min: a.min, Max: a.max, N: a.n})
	}
	return out
}

// SealHotHistory copies aged SQLite ticks into DuckDB, then drops SQLite rows
// older than liveHotRetention. Live enqueue does not wait on this.
func (s *Store) SealHotHistory(ctx context.Context) error {
	now := time.Now()
	return s.sealAndPruneHot(ctx, now.Add(-hotSealAge).UnixMilli(), now.Add(-liveHotRetention).UnixMilli())
}

func (s *Store) sealAndPruneHot(ctx context.Context, sealBefore, pruneBefore int64) error {
	if s.hot == nil {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ticks, err := s.loadUnsealedHotTicks(ctx, sealBefore, hotSealChunk)
		if err != nil {
			return err
		}
		if len(ticks) == 0 {
			break
		}
		if _, err := s.recordHistoryBatches(ctx, ticks, 0); err != nil {
			return err
		}
		ids := make([]string, len(ticks))
		for i, t := range ticks {
			ids[i] = t.id
		}
		if err := s.markHotTicksSealed(ctx, ids); err != nil {
			return err
		}
		if len(ticks) < hotSealChunk {
			break
		}
	}
	return s.pruneHotHistory(ctx, pruneBefore)
}

func (s *Store) loadUnsealedHotTicks(ctx context.Context, beforeMs int64, limit int) ([]historyBatch, error) {
	s.hotWriteMu.Lock()
	defer s.hotWriteMu.Unlock()
	rows, err := s.hot.QueryContext(ctx, `SELECT id, hash, payload FROM hot_ticks WHERE sealed=0 AND ts_ms < ? ORDER BY ts_ms ASC LIMIT ?`, beforeMs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]historyBatch, 0, limit)
	for rows.Next() {
		var b historyBatch
		var payload string
		if err := rows.Scan(&b.id, &b.hash, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &b.payload); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) markHotTicksSealed(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	s.hotWriteMu.Lock()
	defer s.hotWriteMu.Unlock()
	tx, err := s.hot.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE hot_ticks SET sealed=1 WHERE id=?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) pruneHotHistory(ctx context.Context, beforeMs int64) error {
	s.hotWriteMu.Lock()
	defer s.hotWriteMu.Unlock()
	tx, err := s.hot.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM history_hot WHERE ts_ms < ?`, beforeMs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ts_samples WHERE ts_ms < ?`, beforeMs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM hot_ticks WHERE sealed=1 AND ts_ms < ?`, beforeMs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, _ = s.hot.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return nil
}

func (s *Store) hotFileInfo() map[string]any {
	info := map[string]any{"engine": "sqlite", "file": filepath.Base(s.hotPath), "retention_h": liveHotRetention.Hours()}
	if s.hotPath == "" {
		return info
	}
	if stat, err := os.Stat(s.hotPath); err == nil {
		info["file_bytes"] = stat.Size()
	} else {
		info["file_bytes"] = int64(0)
	}
	wal := s.hotPath + "-wal"
	if stat, err := os.Stat(wal); err == nil {
		info["wal_bytes"] = stat.Size()
	} else {
		info["wal_bytes"] = int64(0)
	}
	return info
}
