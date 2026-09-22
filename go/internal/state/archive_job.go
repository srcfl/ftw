package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"time"
)

// Scratch and its cursor commit together. A power loss may repeat the last
// WAL transaction, but can never skip input. Originals survive until verified
// publication; scratch is excluded from portable backups.
type archiveJob struct {
	Phase                                string
	ExistingRows, CopiedRows, PrunedRows int64
	Ts, DriverID, MetricID               int64
	Driver, Metric                       string
	ThroughMS                            int64
	FromMS                               int64
	ArchiveSHA                           string
	SourceSHA                            string
}

type archiveTurnKey struct{}

var errArchiveTurnComplete = errors.New("archive turn complete")

// Yield only after a durable cursor. Publishing a complete Parquet file may
// take longer than a turn on an SD card, but remains cancellable by backup.
func archiveCheckpoint(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Value(archiveTurnKey{}).(time.Time); ok && time.Now().After(deadline) {
		return errArchiveTurnComplete
	}
	return nil
}

func checkArchiveSource(ctx context.Context, db *sql.DB, path, table string, j archiveJob) (archiveJob, error) {
	if j.Phase == "prune" {
		return j, nil
	}
	digest, err := historyFileHashContext(ctx, path)
	if errors.Is(err, os.ErrNotExist) {
		digest, err = "absent", nil
	}
	if err != nil {
		return j, err
	}
	changed := j.SourceSHA != "" && j.SourceSHA != digest
	if changed {
		// No source has been pruned yet. Rebuild only the derived scratch if
		// an import replaced the archive between maintenance turns.
		j = archiveJob{Phase: "existing"}
	}
	if j.SourceSHA == digest {
		return j, nil
	}
	j.SourceSHA = digest
	err = commitArchiveJob(ctx, db, j, func(tx *sql.Tx) error {
		if !changed {
			return nil
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM "+table)
		return err
	})
	return j, err
}

func openArchiveJob(ctx context.Context, path, schema string) (*sql.DB, archiveJob, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-2048)&_pragma=temp_store(FILE)")
	if err != nil {
		return nil, archiveJob{}, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{schema, `CREATE TABLE IF NOT EXISTS archive_job(id INTEGER PRIMARY KEY, progress TEXT NOT NULL)`} {
		if _, err = db.ExecContext(ctx, q); err != nil {
			db.Close()
			return nil, archiveJob{}, err
		}
	}
	j := archiveJob{Phase: "existing"}
	var raw string
	err = db.QueryRowContext(ctx, `SELECT progress FROM archive_job WHERE id=1`).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &j)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, j, err
	}
	return db, j, nil
}

func saveArchiveJob(ctx context.Context, tx *sql.Tx, j archiveJob) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO archive_job VALUES(1,?) ON CONFLICT(id) DO UPDATE SET progress=excluded.progress`, string(raw))
	return err
}

func commitArchiveJob(ctx context.Context, db *sql.DB, j archiveJob, write func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if write != nil {
		if err := write(tx); err != nil {
			return err
		}
	}
	if err := saveArchiveJob(ctx, tx, j); err != nil {
		return err
	}
	return tx.Commit()
}

func removeArchiveJob(db *sql.DB, path string) error {
	if err := db.Close(); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) archiveProgress(file, operation string, done, total int64) {
	s.maintenanceStatusMu.Lock()
	defer s.maintenanceStatusMu.Unlock()
	s.maintenanceStatus.File = file
	s.maintenanceStatus.Operation = operation
	s.maintenanceStatus.RowsDone = done
	s.maintenanceStatus.RowsTotal = total
	if s.maintenanceStatus.Work == nil {
		s.maintenanceStatus.Work = make(map[string]HistoryMaintenanceProgress)
	}
	previous := s.maintenanceStatus.Work[s.maintenanceStatus.Phase]
	if previous.File != file || previous.Operation != operation || previous.RowsDone != done || previous.RowsTotal != total {
		previous = HistoryMaintenanceProgress{File: file, Operation: operation, RowsDone: done, RowsTotal: total, UpdatedMS: time.Now().UnixMilli()}
		s.maintenanceStatus.Work[s.maintenanceStatus.Phase] = previous
	}
	s.maintenanceStatus.UpdatedMS = previous.UpdatedMS
}

func (s *Store) maintenanceYield(ctx context.Context) func() error {
	last := time.Now()
	return func() error {
		if err := archiveCheckpoint(ctx); err != nil {
			return err
		}
		if time.Since(last) >= time.Second || s.HistoryWriterStatus().Pending >= historyCommitMaxTicks/2 {
			if err := pauseMaintenance(ctx); err != nil {
				return err
			}
			last = time.Now()
		}
		return nil
	}
}
