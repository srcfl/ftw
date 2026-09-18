package state

// RetireCalendarProfile ends the old calendar's persisted away selection once.
// The marker and selection commit together. Model weights and old forecasts
// remain unchanged, and later manual profile choices survive each restart.
func (s *Store) RetireCalendarProfile() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT OR IGNORE INTO config (key, value) VALUES ('calendar/retired_v1', '1')`)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 0 {
		if _, err := tx.Exec(`INSERT INTO config (key, value) VALUES ('loadmodel/profile', 'home') ON CONFLICT(key) DO UPDATE SET value = excluded.value`); err != nil {
			return err
		}
	}
	return tx.Commit()
}
