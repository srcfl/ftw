package state

// RecordDriverCommandResult appends one host-completed v2 result and keeps a
// bounded local audit. The caller supplies the already validated JSON result.
func (s *Store) RecordDriverCommandResult(id, driverName, command, status, code string, completedAtMS int64, resultJSON []byte) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO driver_command_results
		(id, driver_name, command, status, code, completed_at_ms, result_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, driverName, command, status, code, completedAtMS, string(resultJSON)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM driver_command_results WHERE id IN (
		SELECT id FROM driver_command_results ORDER BY completed_at_ms DESC LIMIT -1 OFFSET 2000
	)`); err != nil {
		return err
	}
	return tx.Commit()
}
