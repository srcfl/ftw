package state

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"
)

// Lifetime totals are sums of the stored energy history, the same rows that
// give the daily totals, so they need no meter of their own. They are for
// counters that only count up, such as Home Assistant's Energy dashboard.
//
// The base row holds the history added for good, up to through_ms. Whole
// quarter hours are added once the stored history runs an hour past them:
// by then the history writer, load estimates and source holds have long
// finished writing them. Each read adds the newer history to the base. The
// floor row holds the highest totals already returned, so a read never
// returns less, also after a restart. Both rows live in history.db beside
// the rows they sum: an empty history file starts them again at zero, and a
// lost base is rebuilt from the stored history. Full backups (the history
// export) and history files rebuilt by older Cores leave the table out;
// the totals then rebuild from the history that came along.
const lifetimeEnergySchema = `CREATE TABLE IF NOT EXISTS energy_lifetime(key TEXT PRIMARY KEY,through_ms INTEGER NOT NULL,import_wh REAL NOT NULL,export_wh REAL NOT NULL,pv_wh REAL NOT NULL,bat_charged_wh REAL NOT NULL,bat_discharged_wh REAL NOT NULL,load_wh REAL NOT NULL) STRICT`

const (
	lifetimeSettle = time.Hour
	// Quarter hours keep both minute rows and the quarter-hour rollup of
	// two-year-old history whole.
	lifetimeStepMS = int64(15 * time.Minute / time.Millisecond)
	// A first read sums at most a week of older history per call, so a box
	// with years of history never runs one long query.
	lifetimeCatchUp = 7 * 24 * time.Hour
)

// LifetimeEnergy is the energy of each flow since the stored history began,
// in Wh. Export and PV are positive amounts, as in DayEnergy.
type LifetimeEnergy struct {
	ImportWh        float64
	ExportWh        float64
	PVWh            float64
	BatChargedWh    float64
	BatDischargedWh float64
	LoadWh          float64
}

func (l LifetimeEnergy) plus(d DayEnergy) LifetimeEnergy {
	l.ImportWh += d.ImportWh
	l.ExportWh += d.ExportWh
	l.PVWh += d.PVWh
	l.BatChargedWh += d.BatChargedWh
	l.BatDischargedWh += d.BatDischargedWh
	l.LoadWh += d.LoadWh
	return l
}

// atLeast raises each flow to o's value. PV can fall a little at night,
// when an inverter draws standby power, and the load estimate of a recent
// range can change; a counter shown to others must not.
func (l LifetimeEnergy) atLeast(o LifetimeEnergy) LifetimeEnergy {
	l.ImportWh = math.Max(l.ImportWh, o.ImportWh)
	l.ExportWh = math.Max(l.ExportWh, o.ExportWh)
	l.PVWh = math.Max(l.PVWh, o.PVWh)
	l.BatChargedWh = math.Max(l.BatChargedWh, o.BatChargedWh)
	l.BatDischargedWh = math.Max(l.BatDischargedWh, o.BatDischargedWh)
	l.LoadWh = math.Max(l.LoadWh, o.LoadWh)
	return l
}

// LifetimeEnergy returns each flow's energy since the stored history began.
// The totals never fall and never count stored history twice, also across
// restarts. ok is false while a read is still adding up older history, one
// week per call, or while a history import runs before the first read.
//
// The stored history, not the wall clock, decides how far history is added
// for good: a clock that runs ahead must not move the base past rows the
// writer has yet to store, and one that runs behind must not hide stored
// rows from the total.
func (s *Store) LifetimeEnergy(now time.Time) (LifetimeEnergy, bool, error) {
	if s == nil || s.history == nil {
		return LifetimeEnergy{}, false, nil
	}
	s.lifetimeMu.Lock()
	defer s.lifetimeMu.Unlock()
	ctx := context.Background()

	base, through, found, err := s.lifetimeRow(ctx, "base")
	if err != nil {
		return LifetimeEnergy{}, false, err
	}
	floor, _, _, err := s.lifetimeRow(ctx, "floor")
	if err != nil {
		return LifetimeEnergy{}, false, err
	}
	newest, stored, err := s.newestEnergyHistoryMS(ctx)
	if err != nil {
		return LifetimeEnergy{}, false, err
	}
	if !stored {
		// Nothing to add up yet. A new box shows zero and saves nothing.
		out := floor.atLeast(s.lifetimeShown)
		s.lifetimeShown = out
		return out, true, nil
	}
	if !found {
		// A first read, or a history file without the row: start at the
		// oldest stored history, once an import has put it all in place.
		// The value already shown stays the least this process returns.
		if m := s.HistoryMigrationStatus(); !m.HistoryComplete && m.State != "failed" {
			return LifetimeEnergy{}, false, nil
		}
		oldest, _, err := s.oldestEnergyHistoryMS(ctx)
		if err != nil {
			return LifetimeEnergy{}, false, err
		}
		base, through = LifetimeEnergy{}, oldest
	}

	nowMS := now.UnixMilli()
	foldTo := bucketStart(min(nowMS, newest)-lifetimeSettle.Milliseconds(), lifetimeStepMS)
	ready := true
	if foldTo > through || !found {
		to := max(foldTo, through)
		if step := bucketStart(through+lifetimeCatchUp.Milliseconds(), lifetimeStepMS); step < to {
			to, ready = step, false
		}
		if to > through {
			d, err := s.DailyEnergy(through, to)
			if err != nil {
				return LifetimeEnergy{}, false, err
			}
			base, through = base.plus(d), to
		}
		floor = floor.atLeast(s.lifetimeShown)
		if err := s.saveLifetimeRows(ctx, lifetimeRowValue{"base", through, base}, lifetimeRowValue{"floor", 0, floor}); err != nil {
			return LifetimeEnergy{}, false, err
		}
	}
	if !ready {
		return LifetimeEnergy{}, false, nil
	}
	// The newest stored row ends at most a minute after it starts.
	tail, err := s.DailyEnergy(through, max(nowMS, newest+ArchiveResolutionMS))
	if err != nil {
		return LifetimeEnergy{}, false, err
	}
	out := base.plus(tail).atLeast(floor).atLeast(s.lifetimeShown)
	s.lifetimeShown = out
	if out.above(floor, lifetimeFloorMarginWh) {
		if err := s.saveLifetimeRows(ctx, lifetimeRowValue{"floor", 0, out}); err != nil {
			return LifetimeEnergy{}, false, err
		}
	}
	return out, true, nil
}

// lifetimeFloorMarginWh is how far a shown total may run ahead of the saved
// floor before the floor is saved between folds. Close saves the rest, so
// only a power cut can lose it, and then at most this much of a flow whose
// stored total fell, such as PV at night.
const lifetimeFloorMarginWh = 50

func (l LifetimeEnergy) above(o LifetimeEnergy, margin float64) bool {
	return l.ImportWh-o.ImportWh >= margin || l.ExportWh-o.ExportWh >= margin || l.PVWh-o.PVWh >= margin ||
		l.BatChargedWh-o.BatChargedWh >= margin || l.BatDischargedWh-o.BatDischargedWh >= margin || l.LoadWh-o.LoadWh >= margin
}

// saveLifetimeFloor saves the totals already shown as the floor, for Close.
func (s *Store) saveLifetimeFloor(ctx context.Context) error {
	if s.history == nil {
		return nil
	}
	s.lifetimeMu.Lock()
	defer s.lifetimeMu.Unlock()
	if s.lifetimeShown == (LifetimeEnergy{}) {
		return nil
	}
	floor, _, _, err := s.lifetimeRow(ctx, "floor")
	if err != nil {
		return err
	}
	if !s.lifetimeShown.above(floor, math.SmallestNonzeroFloat64) {
		return nil
	}
	return s.saveLifetimeRows(ctx, lifetimeRowValue{"floor", 0, floor.atLeast(s.lifetimeShown)})
}

func (s *Store) lifetimeRow(ctx context.Context, key string) (LifetimeEnergy, int64, bool, error) {
	var l LifetimeEnergy
	var through int64
	err := s.history.QueryRowContext(ctx, `SELECT through_ms,import_wh,export_wh,pv_wh,bat_charged_wh,bat_discharged_wh,load_wh FROM energy_lifetime WHERE key=?`, key).
		Scan(&through, &l.ImportWh, &l.ExportWh, &l.PVWh, &l.BatChargedWh, &l.BatDischargedWh, &l.LoadWh)
	if errors.Is(err, sql.ErrNoRows) {
		return LifetimeEnergy{}, 0, false, nil
	}
	if err != nil {
		return LifetimeEnergy{}, 0, false, err
	}
	return l, through, true, nil
}

type lifetimeRowValue struct {
	key     string
	through int64
	l       LifetimeEnergy
}

// saveLifetimeRows writes rows in one transaction: a fold saves the base and
// the floor together, so a restart finds a floor that covers the base.
func (s *Store) saveLifetimeRows(ctx context.Context, rows ...lifetimeRowValue) error {
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, end, err := s.beginHistoryWrite(ctx)
	if err != nil {
		return err
	}
	defer end()
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO energy_lifetime VALUES(?,?,?,?,?,?,?,?)
 ON CONFLICT(key) DO UPDATE SET through_ms=excluded.through_ms,import_wh=excluded.import_wh,export_wh=excluded.export_wh,pv_wh=excluded.pv_wh,
 bat_charged_wh=excluded.bat_charged_wh,bat_discharged_wh=excluded.bat_discharged_wh,load_wh=excluded.load_wh`,
			row.key, row.through, row.l.ImportWh, row.l.ExportWh, row.l.PVWh, row.l.BatChargedWh, row.l.BatDischargedWh, row.l.LoadWh); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// oldestEnergyHistoryMS is the start of the oldest stored energy evidence:
// minute or quarter-hour energy rows, or the raw ticks older Cores kept.
func (s *Store) oldestEnergyHistoryMS(ctx context.Context) (int64, bool, error) {
	var oldest sql.NullInt64
	err := s.history.QueryRowContext(ctx, `SELECT MIN(m) FROM (
 SELECT MIN(start_ms) AS m FROM history_site_energy UNION ALL SELECT MIN(start_ms) FROM history_site_energy_partial
 UNION ALL SELECT MIN(start_ms) FROM `+sourceEnergyTable+`
 UNION ALL SELECT MIN(ts_ms) FROM history_hot UNION ALL SELECT MIN(ts_ms) FROM history_warm UNION ALL SELECT MIN(ts_ms) FROM history_cold)`).Scan(&oldest)
	if err != nil {
		return 0, false, err
	}
	return oldest.Int64, oldest.Valid, nil
}

// newestEnergyHistoryMS is the start of the newest stored energy evidence.
// Each table's primary key or index leads with the time, so this is cheap.
func (s *Store) newestEnergyHistoryMS(ctx context.Context) (int64, bool, error) {
	var newest sql.NullInt64
	err := s.history.QueryRowContext(ctx, `SELECT MAX(m) FROM (
 SELECT MAX(start_ms) AS m FROM history_site_energy UNION ALL SELECT MAX(start_ms) FROM history_site_energy_partial
 UNION ALL SELECT MAX(start_ms) FROM `+sourceEnergyTable+`
 UNION ALL SELECT MAX(ts_ms) FROM history_hot UNION ALL SELECT MAX(ts_ms) FROM history_warm UNION ALL SELECT MAX(ts_ms) FROM history_cold)`).Scan(&newest)
	if err != nil {
		return 0, false, err
	}
	return newest.Int64, newest.Valid, nil
}
