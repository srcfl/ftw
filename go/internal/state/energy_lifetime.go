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
// quarter hours are added once they are an hour old: by then the history
// writer, load estimates and source holds have long finished writing them.
// Each read adds the newer history to the base. The floor row holds the
// highest totals already returned, so a read never returns less, also after
// a restart. Both rows live in history.db beside the rows they sum: an empty
// history file starts them again at zero, and a lost base is rebuilt from
// the stored history.
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
// restarts. ok is false while the first read is still adding up older
// history, one week per call, or while a history import runs.
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
	foldTo := bucketStart(now.Add(-lifetimeSettle).UnixMilli(), lifetimeStepMS)
	if !found {
		// A first read, or a history file without the row: start at the
		// oldest stored history, once an import has put it all in place.
		if m := s.HistoryMigrationStatus(); !m.HistoryComplete && m.State != "failed" {
			return LifetimeEnergy{}, false, nil
		}
		oldest, ok, err := s.oldestEnergyHistoryMS(ctx)
		if err != nil {
			return LifetimeEnergy{}, false, err
		}
		through = foldTo
		if ok && oldest < foldTo {
			through = oldest
		}
		base = LifetimeEnergy{}
		s.lifetimeShown = LifetimeEnergy{}
	}

	ready := true
	if foldTo > through || !found {
		to := foldTo
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
		if err := s.saveLifetime(ctx, through, base, floor); err != nil {
			return LifetimeEnergy{}, false, err
		}
	}
	if !ready {
		return LifetimeEnergy{}, false, nil
	}
	tail, err := s.DailyEnergy(through, now.UnixMilli())
	if err != nil {
		return LifetimeEnergy{}, false, err
	}
	out := base.plus(tail).atLeast(floor).atLeast(s.lifetimeShown)
	s.lifetimeShown = out
	return out, true, nil
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

// saveLifetime writes the base and the floor in one transaction, so a
// restart finds a floor that covers what the base already holds.
func (s *Store) saveLifetime(ctx context.Context, through int64, base, floor LifetimeEnergy) error {
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, end, err := s.beginHistoryWrite(ctx)
	if err != nil {
		return err
	}
	defer end()
	for _, row := range []struct {
		key     string
		through int64
		l       LifetimeEnergy
	}{{"base", through, base}, {"floor", 0, floor}} {
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
