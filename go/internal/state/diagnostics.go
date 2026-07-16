package state

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DiagnosticsRecentRetention is how long planner snapshots stay queryable
// in SQLite before rolling off to daily Parquet files. The time-travel UI
// falls through to Parquet transparently for anything older, so this only
// buys query latency on recent incidents — and the rows are heavy: a real
// site measured ~85 kB JSON per replan ≈ 485 MB in SQLite at 30 days,
// which also ballooned every state snapshot. 7 days keeps the common
// debugging window fast at ~115 MB.
const DiagnosticsRecentRetention = 7 * 24 * time.Hour

// DiagnosticSummary is the light-weight row the timeline UI consumes. No
// full JSON blob — the UI fetches that on demand via LoadDiagnosticAt.
type DiagnosticSummary struct {
	TsMs         int64   `json:"ts_ms"`
	Reason       string  `json:"reason"`
	Zone         string  `json:"zone"`
	TotalCostOre float64 `json:"total_cost_ore"`
	HorizonSlots int     `json:"horizon_slots"`
}

// DiagnosticRow is the full persisted record — used by the detail view
// and the rolloff path.
type DiagnosticRow struct {
	DiagnosticSummary
	JSON string `json:"json"`
}

// SaveDiagnostic persists one planner replan snapshot. Upserts on conflict
// so a replan that happens to reuse a ts_ms (should not in practice —
// time.Now().UnixMilli() has ms precision and replans are minutes apart)
// overwrites cleanly rather than failing.
func (s *Store) SaveDiagnostic(tsMs int64, reason, zone string,
	totalCostOre float64, horizonSlots int, js string) error {
	if tsMs <= 0 {
		return fmt.Errorf("SaveDiagnostic: ts_ms must be positive")
	}
	_, err := s.db.Exec(
		`INSERT INTO planner_diagnostics
		 (ts_ms, reason, zone, total_cost_ore, horizon_slots, json)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (ts_ms) DO UPDATE SET
		   reason=excluded.reason, zone=excluded.zone,
		   total_cost_ore=excluded.total_cost_ore,
		   horizon_slots=excluded.horizon_slots,
		   json=excluded.json`,
		tsMs, reason, zone, totalCostOre, horizonSlots, js)
	return err
}

// LoadDiagnosticsInRange returns summaries for replans in [sinceMs, untilMs],
// newest first. Caller-chosen `limit` caps the result; 0 = no limit. Used
// by the timeline list in the UI.
func (s *Store) LoadDiagnosticsInRange(sinceMs, untilMs int64,
	limit int) ([]DiagnosticSummary, error) {
	var args []any
	q := `SELECT ts_ms, reason, zone, total_cost_ore, horizon_slots
	      FROM planner_diagnostics
	      WHERE ts_ms >= ? AND ts_ms <= ?
	      ORDER BY ts_ms DESC`
	args = append(args, sinceMs, untilMs)
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DiagnosticSummary, 0, 32)
	for rows.Next() {
		var r DiagnosticSummary
		if err := rows.Scan(&r.TsMs, &r.Reason, &r.Zone,
			&r.TotalCostOre, &r.HorizonSlots); err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadDiagnosticAt returns the snapshot that was active at tsMs — the
// most recent one with ts_ms <= tsMs. This is the correct semantics for
// "what plan was driving the EMS at this moment?": if a replan ran at
// 02:00:00 and another at 02:15:00, then asking for 02:07:00 returns the
// 02:00 snapshot. Returns (nil, nil) when no snapshot is ≤ tsMs.
func (s *Store) LoadDiagnosticAt(tsMs int64) (*DiagnosticRow, error) {
	row := s.db.QueryRow(
		`SELECT ts_ms, reason, zone, total_cost_ore, horizon_slots, json
		 FROM planner_diagnostics
		 WHERE ts_ms <= ?
		 ORDER BY ts_ms DESC
		 LIMIT 1`, tsMs)
	var r DiagnosticRow
	err := row.Scan(&r.TsMs, &r.Reason, &r.Zone,
		&r.TotalCostOre, &r.HorizonSlots, &r.JSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteDiagnosticsBefore drops all snapshots with ts_ms < cutoffMs. Used
// by the rolloff path after successfully writing Parquet, and by a
// periodic retention prune if cold storage is disabled.
func (s *Store) DeleteDiagnosticsBefore(ctx context.Context, cutoffMs int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM planner_diagnostics WHERE ts_ms < ?`, cutoffMs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DiagnosticsBefore streams rows older than cutoffMs in batches — used by
// the Parquet rolloff to avoid loading 30 days of snapshots into memory
// at once. Visitor returning an error stops the scan.
func (s *Store) DiagnosticsBefore(ctx context.Context, cutoffMs int64,
	batchSize int, visit func(batch []DiagnosticRow) error) error {
	if batchSize <= 0 {
		batchSize = 256
	}
	// Keyset pagination on ts_ms — PK ordering means this is a simple
	// forward scan without offset scanning overhead.
	var lastTs int64 = -1
	for {
		q := `SELECT ts_ms, reason, zone, total_cost_ore, horizon_slots, json
		      FROM planner_diagnostics
		      WHERE ts_ms < ? AND ts_ms > ?
		      ORDER BY ts_ms ASC
		      LIMIT ?`
		rows, err := s.db.QueryContext(ctx, q, cutoffMs, lastTs, batchSize)
		if err != nil {
			return err
		}
		batch := make([]DiagnosticRow, 0, batchSize)
		for rows.Next() {
			var r DiagnosticRow
			if err := rows.Scan(&r.TsMs, &r.Reason, &r.Zone,
				&r.TotalCostOre, &r.HorizonSlots, &r.JSON); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		if err := visit(batch); err != nil {
			return err
		}
		lastTs = batch[len(batch)-1].TsMs
		if len(batch) < batchSize {
			return nil
		}
	}
}
