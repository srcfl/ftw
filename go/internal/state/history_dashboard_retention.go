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
		// Read only the source range, including its newest detail. Joining the
		// MAX(last_ms) back to this table made SQLite scan every recent row
		// for each bucket, even when the write batch had shrunk to one minute.
		var points []dashboardRollup
		err := s.rollupTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var err error
			points, err = readDashboardRollup(ctx, tx, source, width, from, to)
			return err
		}, func(ctx context.Context, tx *sql.Tx) error {
			for _, p := range points {
				n := float64(p.n)
				if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO history_dashboard VALUES(?,?,?,?,?,?,?,?,?,?,?,X'')`,
					p.start, width, p.first, p.last, p.n, p.grid/n, p.pv/n, p.bat/n, p.load/n, p.soc/n, p.json); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM history_dashboard WHERE resolution_ms=? AND start_ms>=? AND start_ms<?`, source, from, to)
			return err
		})
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			if limit == 1 {
				return fmt.Errorf("dashboard bucket %d cannot commit within its write budget: %w", from, err)
			}
			limit = max(1, limit/2)
		} else if err != nil && !historyWriteBusy(err) {
			return err
		}
		if err := pauseMaintenance(ctx); err != nil {
			return err
		}
	}
}

type dashboardRollup struct {
	start, first, last, n    int64
	grid, pv, bat, load, soc float64
	json                     string
}

func readDashboardRollup(ctx context.Context, tx *sql.Tx, source, width, from, to int64) ([]dashboardRollup, error) {
	rows, err := tx.QueryContext(ctx, `SELECT start_ms,first_ms,last_ms,n,grid_w,pv_w,bat_w,load_w,bat_soc,json
 FROM history_dashboard WHERE resolution_ms=? AND start_ms>=? AND start_ms<? ORDER BY start_ms`, source, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var points []dashboardRollup
	for rows.Next() {
		var p dashboardRollup
		if err := rows.Scan(&p.start, &p.first, &p.last, &p.n, &p.grid, &p.pv, &p.bat, &p.load, &p.soc, &p.json); err != nil {
			return nil, err
		}
		if p.n <= 0 {
			return nil, errors.New("dashboard bucket has no samples")
		}
		p.start = bucketStart(p.start, width)
		n := float64(p.n)
		p.grid, p.pv, p.bat, p.load, p.soc = p.grid*n, p.pv*n, p.bat*n, p.load*n, p.soc*n
		if len(points) == 0 || points[len(points)-1].start != p.start {
			points = append(points, p)
			continue
		}
		a := &points[len(points)-1]
		a.first = min(a.first, p.first)
		if p.last >= a.last {
			a.last, a.json = p.last, p.json
		}
		a.n += p.n
		a.grid += p.grid
		a.pv += p.pv
		a.bat += p.bat
		a.load += p.load
		a.soc += p.soc
	}
	return points, rows.Err()
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
