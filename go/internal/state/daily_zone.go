package state

import "context"

// dailyZonePrefix marks, in history_migrations, the household zone the cached
// local-day energy was computed on.
const dailyZonePrefix = "daily-zone:"

// KeepDailyZone ties the cached local-day energy to the household zone.
//
// Day keys are dates on the household clock, so a day cached on another clock
// covers other hours. A new zone clears the cache, and each day is computed
// again from the UTC buckets on its next read; cleared reports that. The
// first call on a box records the zone and keeps the rows, which were cached
// on the clock it already runs on.
func (s *Store) KeepDailyZone(ctx context.Context, zone string) (cleared bool, err error) {
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT name FROM history_migrations WHERE name LIKE ?`, dailyZonePrefix+"%")
	if err != nil {
		return false, err
	}
	var marks []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return false, err
		}
		marks = append(marks, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	want := dailyZonePrefix + zone
	if len(marks) == 1 && marks[0] == want {
		return false, nil
	}
	cleared = len(marks) > 0
	if cleared {
		for _, table := range []string{"energy_daily", "energy_daily_partial", "history_daily_quality"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
				return false, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM history_migrations WHERE name LIKE ?`, dailyZonePrefix+"%"); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES(?)`, want); err != nil {
		return false, err
	}
	return cleared, tx.Commit()
}
