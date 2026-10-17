package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
)

// Estimates supplement, never overwrite, the original measurements and quality.
// Signed residuals are integrated before taking a daily total: clipping each
// negative instantaneous balance biases the total upwards. Forecast training
// and control continue to reject those instantaneous samples.
const loadEstimateSchema = `CREATE TABLE IF NOT EXISTS history_load_estimates (
 start_ms INTEGER NOT NULL,resolution_ms INTEGER NOT NULL,covered_ms INTEGER NOT NULL,
 load_wh REAL NOT NULL,method TEXT NOT NULL,PRIMARY KEY(start_ms,resolution_ms)) WITHOUT ROWID`

func residualHouseholdW(p *HistoryPoint) (float64, bool) {
	if p.Missing != 0 && p.Missing != MissingLoad|BalanceMismatch {
		return 0, false
	}
	var evidence struct {
		EV     *float64 `json:"ev_w"`
		V2X    *float64 `json:"v2x_w"`
		Reason string   `json:"history_reason"`
	}
	if json.Unmarshal([]byte(p.JSON), &evidence) != nil || evidence.EV == nil || evidence.V2X == nil {
		return 0, false
	}
	if p.Missing != 0 && evidence.Reason != "invalid_balance" {
		return 0, false
	}
	w := p.GridW - p.PVW - p.BatW - *evidence.EV - *evidence.V2X
	return w, !math.IsNaN(w) && !math.IsInf(w, 0)
}

func addSiteIntervalEstimateTx(ctx context.Context, tx *sql.Tx, previous int64, p *HistoryPoint, previousMissing HistoryMissing, previousEligible bool) error {
	// An offline, duplicate, skewed or otherwise missing source is not repaired
	// by interpreting it as zero. Both endpoints need usable source evidence.
	if p.Missing|previousMissing != MissingLoad|BalanceMismatch || !previousEligible {
		return nil
	}
	w, ok := residualHouseholdW(p)
	if !ok {
		return nil
	}
	for from := previous; from < p.TsMs; {
		start := bucketStart(from, ArchiveResolutionMS)
		to := min(p.TsMs, start+ArchiveResolutionMS)
		covered := to - from
		_, err := tx.ExecContext(ctx, `INSERT INTO history_load_estimates VALUES(?,?,?,?,?)
   ON CONFLICT(start_ms,resolution_ms) DO UPDATE SET covered_ms=covered_ms+excluded.covered_ms,load_wh=load_wh+excluded.load_wh`, start, ArchiveResolutionMS, covered, w*float64(covered)/3600000, "signed_source_balance_v1")
		if err != nil {
			return err
		}
		from = to
	}
	return nil
}

// HistoryEnergyCoverage reports actual integrated time, excluding writer stalls
// and offline gaps. It does not extrapolate energy into unrecorded periods.
func (s *Store) HistoryEnergyCoverage(ctx context.Context, from, to int64) (int64, error) {
	var covered int64
	err := s.history.QueryRowContext(ctx, `SELECT COALESCE(SUM(covered_ms),0) FROM (
 SELECT start_ms,first_ms,last_ms,covered_ms FROM history_site_energy
 UNION ALL SELECT start_ms,first_ms,last_ms,covered_ms FROM history_site_energy_partial
 ) WHERE start_ms>=? AND start_ms<? AND first_ms>=? AND last_ms<=?`, from, to, from, to).Scan(&covered)
	return covered, err
}
