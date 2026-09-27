package state

import (
	"context"
	"database/sql"
)

const HotHistoryFilename = "history-hot.db" // beta migration source only

func (s *Store) hotEarliestMs(ctx context.Context) (int64, bool, error) {
	if s.history == nil {
		return 0, false, nil
	}
	var ts sql.NullInt64
	if err := s.history.QueryRowContext(ctx, `SELECT MIN(ts_ms) FROM history_hot`).Scan(&ts); err != nil {
		return 0, false, err
	}
	if !ts.Valid {
		return 0, false, nil
	}
	return ts.Int64, true, nil
}

func (s *Store) dailyEnergyFromHot(ctx context.Context, sinceMs, untilMs int64) (DayEnergy, error) {
	if s.history == nil {
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
	err := s.history.QueryRowContext(ctx, q, sinceMs, untilMs, maxCostIntegrationGap.Milliseconds()).Scan(
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
