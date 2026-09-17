package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *Store) maintainDashboard(ctx context.Context, now time.Time) error {
	cutoff := s.reserveAggregateCutoff(now.Add(-AggregateRecentRetention).UnixMilli())
	if err := s.rollDashboard(ctx, HistoryResolutionMS, ArchiveResolutionMS, cutoff); err != nil {
		return err
	}
	if err := s.rollDashboard(ctx, ArchiveResolutionMS, OldArchiveResolutionMS, now.Add(-AggregateMinuteRetention).UnixMilli()); err != nil {
		return err
	}
	if err := s.rollSiteEnergy(ctx, now.Add(-AggregateRetention).UnixMilli()); err != nil {
		return err
	}
	for {
		var n int64
		err := s.writeArchiveBatch(ctx, func(ctx context.Context, tx *sql.Tx) error {
			result, err := tx.ExecContext(ctx, `DELETE FROM history_dashboard WHERE (start_ms,resolution_ms) IN (SELECT start_ms,resolution_ms FROM history_dashboard WHERE last_ms<? ORDER BY start_ms LIMIT 128)`, now.Add(-AggregateRetention).UnixMilli())
			if err != nil {
				return err
			}
			n, err = result.RowsAffected()
			return err
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

func (s *Store) rollDashboard(ctx context.Context, source, width, cutoff int64) error {
	cutoff = bucketStart(cutoff, width)
	limit := int64(128)
	for {
		var first sql.NullInt64
		if err := s.history.QueryRowContext(ctx, `SELECT MIN(start_ms) FROM history_dashboard WHERE resolution_ms=? AND start_ms<?`, source, cutoff).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return nil
		}
		from := bucketStart(first.Int64, width)
		to := min(cutoff, from+limit*width)
		// One statement reads and writes a consistent source set; the transaction
		// also removes that set. A retry replaces only complete target buckets.
		err := s.tryArchiveBatch(ctx, nil, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `WITH a AS (
 SELECT (start_ms/?)*? AS start,MIN(first_ms) AS first,MAX(last_ms) AS last,SUM(n) AS count,
 SUM(grid_w*n)/SUM(n) AS grid,SUM(pv_w*n)/SUM(n) AS pv,SUM(bat_w*n)/SUM(n) AS bat,SUM(load_w*n)/SUM(n) AS load,SUM(bat_soc*n)/SUM(n) AS soc
 FROM history_dashboard WHERE resolution_ms=? AND start_ms>=? AND start_ms<? GROUP BY start_ms/?)
 INSERT OR REPLACE INTO history_dashboard SELECT a.start,?,a.first,a.last,a.count,a.grid,a.pv,a.bat,a.load,a.soc,newest.json,X''
 FROM a JOIN history_dashboard newest ON newest.resolution_ms=? AND newest.last_ms=a.last`, width, width, source, from, to, width, width, source)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM history_dashboard WHERE resolution_ms=? AND start_ms>=? AND start_ms<?`, source, from, to)
			return err
		})
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			limit = max(1, limit/2)
		} else if err != nil && !historyWriteBusy(err) {
			return err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

// Keep minute accounting for two years, then quarter-hour energy totals.
// Unlike UTC daily buckets, quarters preserve local midnight boundaries and
// normal tariff periods without spreading energy across a missing interval.
func (s *Store) rollSiteEnergy(ctx context.Context, cutoff int64) error {
	width := int64((15 * time.Minute).Milliseconds())
	cutoff = bucketStart(cutoff, width)
	for {
		var first sql.NullInt64
		if err := s.history.QueryRowContext(ctx, `SELECT MIN(start_ms) FROM history_site_energy WHERE resolution_ms=? AND start_ms<?`, ArchiveResolutionMS, cutoff).Scan(&first); err != nil {
			return err
		}
		if !first.Valid {
			return nil
		}
		from := bucketStart(first.Int64, width)
		to := from + width
		// At most fifteen minute rows; a read snapshot does not hold the
		// live writer while it computes its small additive result.
		var r siteEnergyRow
		err := s.rollupTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT MIN(first_ms),MAX(last_ms),SUM(covered_ms),SUM(n),SUM(import_wh),SUM(export_wh),SUM(pv_wh),SUM(bat_charge_wh),SUM(bat_discharge_wh),SUM(load_wh),SUM(ev_wh) FROM history_site_energy WHERE resolution_ms=? AND start_ms>=? AND start_ms<?`, ArchiveResolutionMS, from, to).Scan(&r.first, &r.last, &r.covered, &r.n, &r.imp, &r.exp, &r.pv, &r.charge, &r.discharge, &r.load, &r.ev)
		}, func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO history_site_energy VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, from, width, r.first, r.last, r.covered, r.n, r.imp, r.exp, r.pv, r.charge, r.discharge, r.load, r.ev)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM history_site_energy WHERE resolution_ms=? AND start_ms>=? AND start_ms<?`, ArchiveResolutionMS, from, to)
			return err
		})
		if err != nil {
			return fmt.Errorf("quarter-hour site energy: %w", err)
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}
