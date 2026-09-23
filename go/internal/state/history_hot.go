package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

const HotHistoryFilename = "history-hot.db" // beta migration source only

func (s *Store) openHotHistory() error                           { s.hot = s.history; s.hotPath = s.historyPath; return nil }
func (s *Store) checkpointLiveHistory(ctx context.Context) error { return s.CheckpointHistory(ctx) }

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

func (s *Store) hotFileInfo() map[string]any {
	info := map[string]any{"engine": "sqlite", "file": filepath.Base(s.hotPath), "retention_h": HotRetention.Hours()}
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
