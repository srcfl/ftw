package state

import (
	"database/sql"
	"time"
)

// ---- Irradiance history (cache.db) ----

// IrradianceRow is one hour of historical horizontal irradiance (W/m²).
// DHIWm2 is nil when the source did not provide a diffuse component.
type IrradianceRow struct {
	SlotTsMs    int64    `json:"slot_ts_ms"`
	GHIWm2      float64  `json:"ghi_wm2"`
	DHIWm2      *float64 `json:"dhi_wm2,omitempty"`
	Source      string   `json:"source"`
	FetchedAtMs int64    `json:"fetched_at_ms"`
}

// SaveIrradiance upserts a batch of historical-irradiance rows (keyed by
// slot_ts_ms). Re-fetching a window overwrites the existing rows.
func (s *Store) SaveIrradiance(rows []IrradianceRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.cache.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO irradiance_history
		(slot_ts_ms, ghi_wm2, dhi_wm2, source, fetched_at_ms)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (slot_ts_ms) DO UPDATE SET
			ghi_wm2 = excluded.ghi_wm2,
			dhi_wm2 = excluded.dhi_wm2,
			source = excluded.source,
			fetched_at_ms = excluded.fetched_at_ms`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(r.SlotTsMs, r.GHIWm2, r.DHIWm2, r.Source, r.FetchedAtMs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadIrradiance returns irradiance rows in [sinceMs, untilMs], ascending.
func (s *Store) LoadIrradiance(sinceMs, untilMs int64) ([]IrradianceRow, error) {
	rows, err := s.cache.Query(`SELECT slot_ts_ms, ghi_wm2, dhi_wm2, source, fetched_at_ms
		FROM irradiance_history
		WHERE slot_ts_ms BETWEEN ? AND ?
		ORDER BY slot_ts_ms ASC`, sinceMs, untilMs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IrradianceRow{}
	for rows.Next() {
		var r IrradianceRow
		if err := rows.Scan(&r.SlotTsMs, &r.GHIWm2, &r.DHIWm2, &r.Source, &r.FetchedAtMs); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- PV performance daily (state.db) ----

// PVPerformanceDay is one day's PV performance score: expected DC energy under
// measured irradiance versus the site's actual generation, plus their ratio.
// PR is nil when expected production was below a meaningful floor (n/a).
type PVPerformanceDay struct {
	Day              string   `json:"day"` // YYYY-MM-DD, local date
	ExpectedWh       float64  `json:"expected_wh"`
	ActualWh         float64  `json:"actual_wh"`
	PR               *float64 `json:"pr,omitempty"`
	StrangDataDateMs *int64   `json:"strang_data_date_ms,omitempty"`
	ComputedAtMs     int64    `json:"computed_at_ms"`
}

// SavePVPerformance upserts one day's PV performance score (keyed by day).
func (s *Store) SavePVPerformance(p PVPerformanceDay) error {
	const q = `
		INSERT INTO pv_performance_daily(
			day, expected_wh, actual_wh, pr, strang_data_date_ms, computed_at_ms
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(day) DO UPDATE SET
			expected_wh         = excluded.expected_wh,
			actual_wh           = excluded.actual_wh,
			pr                  = excluded.pr,
			strang_data_date_ms = excluded.strang_data_date_ms,
			computed_at_ms      = excluded.computed_at_ms
	`
	_, err := s.db.Exec(q, p.Day, p.ExpectedWh, p.ActualWh, p.PR, p.StrangDataDateMs, time.Now().UnixMilli())
	return err
}

// LoadPVPerformanceDay returns one day's score, or ok=false on a cache miss.
func (s *Store) LoadPVPerformanceDay(day string) (PVPerformanceDay, bool, error) {
	const q = `SELECT day, expected_wh, actual_wh, pr, strang_data_date_ms, computed_at_ms
		FROM pv_performance_daily WHERE day = ?`
	var p PVPerformanceDay
	err := s.db.QueryRow(q, day).Scan(&p.Day, &p.ExpectedWh, &p.ActualWh, &p.PR, &p.StrangDataDateMs, &p.ComputedAtMs)
	if err == sql.ErrNoRows {
		return PVPerformanceDay{}, false, nil
	}
	if err != nil {
		return PVPerformanceDay{}, false, err
	}
	return p, true, nil
}

// LoadPVPerformance returns scored days in [sinceDay, untilDay] (inclusive,
// YYYY-MM-DD string compare), ascending by day.
func (s *Store) LoadPVPerformance(sinceDay, untilDay string) ([]PVPerformanceDay, error) {
	rows, err := s.db.Query(`SELECT day, expected_wh, actual_wh, pr, strang_data_date_ms, computed_at_ms
		FROM pv_performance_daily
		WHERE day BETWEEN ? AND ?
		ORDER BY day ASC`, sinceDay, untilDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PVPerformanceDay{}
	for rows.Next() {
		var p PVPerformanceDay
		if err := rows.Scan(&p.Day, &p.ExpectedWh, &p.ActualWh, &p.PR, &p.StrangDataDateMs, &p.ComputedAtMs); err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
