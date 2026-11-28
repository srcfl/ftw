// Package state — snapshot_state.go: periodic recovery snapshot of state.db.
//
// state.db holds trained models, configuration and device identity. If the SD card corrupts it, openChecked restores
// from the snapshot this file maintains. cache.db needs no snapshot — it's
// re-fetchable, so corruption there just rebuilds empty.
package state

import (
	"fmt"
	"os"
)

// statePath returns the on-disk path of the precious DB, as SQLite reports it
// via `PRAGMA database_list` (the "main" entry).
func (s *Store) statePath() (string, error) {
	rows, err := s.db.Query("PRAGMA database_list")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" {
			return file, nil
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("statePath: no main database")
}

// SnapshotState writes a fresh "<state.db>.snapshot" recovery copy atomically:
// snapshot to a temp file, verify it with quick_check, then rename over the
// previous snapshot. Reuses SnapshotTo, which excludes the bulky time-series
// tables and paces its writes behind live commits. The snapshot retains the
// history generation binding; recovering a missing history file requires a
// full backup.
func (s *Store) SnapshotState() error {
	main, err := s.statePath()
	if err != nil {
		return err
	}
	final := main + ".snapshot"
	tmp := main + ".snapshot.tmp"
	// SnapshotTo refuses an existing file. A journal left by an interrupted
	// copy would otherwise be rolled back into the next one.
	removeSnapshotTemp(tmp)

	if err := s.SnapshotTo(tmp); err != nil {
		removeSnapshotTemp(tmp)
		return fmt.Errorf("snapshot write: %w", err)
	}
	// Verify the temp snapshot before promoting it — never overwrite a good
	// snapshot with a corrupt one. The check reads only the new file and
	// holds no lock on state.db or history.db.
	vdb, err := openRaw(tmp)
	if err != nil {
		removeSnapshotTemp(tmp)
		return err
	}
	ok, qerr := quickCheck(vdb)
	vdb.Close()
	if qerr != nil || !ok {
		removeSnapshotTemp(tmp)
		return fmt.Errorf("snapshot failed integrity check")
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("snapshot promote: %w", err)
	}
	return nil
}

func removeSnapshotTemp(tmp string) {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		_ = os.Remove(tmp + suffix)
	}
}
