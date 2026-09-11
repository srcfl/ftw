package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// HistoryMigrationStatus describes historical coverage, independently of Core
// readiness and live-writer durability. Unknown totals and bounds are omitted.
type HistoryMigrationStatus struct {
	State                  string `json:"state"`
	Phase                  string `json:"phase"`
	HistoryComplete        bool   `json:"history_complete"`
	FilesDone              int    `json:"files_done"`
	FilesTotal             int    `json:"files_total"`
	RowsDone               int64  `json:"rows_done"`
	RowsTotal              int64  `json:"rows_total,omitempty"`
	CurrentSource          string `json:"current_source,omitempty"`
	CurrentSourceRowsDone  int64  `json:"current_source_rows_done"`
	CurrentSourceRowsTotal int64  `json:"current_source_rows_total,omitempty"`
	StartedAtMS            int64  `json:"started_at_ms"`
	UpdatedAtMS            int64  `json:"updated_at_ms"`
	LastError              string `json:"last_error,omitempty"`
	IncompleteFromMS       *int64 `json:"incomplete_from_ms,omitempty"`
	IncompleteUntilMS      *int64 `json:"incomplete_until_ms,omitempty"`
	// Bytes count compressed Parquet source files only. A partial file uses
	// its verified row fraction; unknown byte totals and ETA are omitted.
	SourceBytesTotal *int64  `json:"source_bytes_total,omitempty"`
	SourceBytesDone  *int64  `json:"source_bytes_done,omitempty"`
	BytesPerSecond   float64 `json:"bytes_per_second,omitempty"`
	ElapsedMS        int64   `json:"elapsed_ms"`
	ETASeconds       *int64  `json:"eta_seconds,omitempty"`
	Activity         string  `json:"activity,omitempty"`
	BytesEstimated   bool    `json:"bytes_estimated"`
}

type historyMigration struct {
	mu           sync.Mutex
	status       HistoryMigrationStatus
	progress     func(HistoryMigrationStatus)
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	byteStarted  time.Time
	bytesThisRun int64
}

func newHistoryMigration(progress func(HistoryMigrationStatus)) *historyMigration {
	ctx, cancel := context.WithCancel(context.Background())
	return &historyMigration{ctx: ctx, cancel: cancel, done: make(chan struct{}), progress: progress}
}

func (m *historyMigration) update(change func(*HistoryMigrationStatus)) {
	m.mu.Lock()
	change(&m.status)
	m.status.UpdatedAtMS = time.Now().UnixMilli()
	st := m.snapshotLocked(time.Now())
	m.mu.Unlock()
	if m.progress != nil {
		m.progress(st)
	}
}

func (s *Store) HistoryMigrationStatus() HistoryMigrationStatus {
	if s.historyMigration == nil {
		return HistoryMigrationStatus{State: "complete", Phase: "complete", HistoryComplete: true}
	}
	m := s.historyMigration
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked(time.Now())
}

func (m *historyMigration) snapshotLocked(now time.Time) HistoryMigrationStatus {
	st := m.status
	end := now.UnixMilli()
	if st.State == "complete" || st.State == "failed" {
		end = st.UpdatedAtMS
	}
	st.ElapsedMS = max(0, end-st.StartedAtMS)
	seconds := now.Sub(m.byteStarted).Seconds()
	if st.State == "running" && st.SourceBytesTotal != nil && st.SourceBytesDone != nil && seconds >= 5 && m.bytesThisRun > 0 {
		st.BytesPerSecond = float64(m.bytesThisRun) / seconds
		if st.Activity == "importing" {
			eta := int64(math.Ceil(float64(max(0, *st.SourceBytesTotal-*st.SourceBytesDone)) / st.BytesPerSecond))
			st.ETASeconds = &eta
		}
	}
	return st
}

func (m *historyMigration) startSourceBytes(total, done *int64) {
	m.update(func(st *HistoryMigrationStatus) {
		st.SourceBytesTotal, st.SourceBytesDone = total, done
		m.byteStarted, m.bytesThisRun = time.Now(), 0
	})
}

func (m *historyMigration) addSourceBytes(delta int64, estimated, newWork bool) {
	m.update(func(st *HistoryMigrationStatus) {
		if st.SourceBytesDone == nil || st.SourceBytesTotal == nil {
			return
		}
		done := min(*st.SourceBytesTotal, *st.SourceBytesDone+max(0, delta))
		st.SourceBytesDone, st.BytesEstimated = &done, estimated
		if newWork {
			m.bytesThisRun += max(0, delta)
		}
	})
}

func (s *Store) historyActivity(activity string) {
	if s.historyMigration != nil {
		s.historyMigration.update(func(st *HistoryMigrationStatus) { st.Activity = activity })
	}
}

func (s *Store) checkpointHistoryImport(ctx context.Context) error {
	previous := s.HistoryMigrationStatus().Activity
	s.historyActivity("checkpointing")
	defer s.historyActivity(previous)
	return s.CheckpointHistory(ctx)
}

func (s *Store) runHistoryMigration(coldDir string) {
	m := s.historyMigration
	defer close(m.done)
	m.update(func(st *HistoryMigrationStatus) {
		st.State, st.Phase, st.Activity = "running", "sqlite", "checking"
		st.CurrentSource = "state.db/ts_samples"
		st.CurrentSourceRowsDone, st.CurrentSourceRowsTotal = 0, 0
	})
	err := s.importSQLiteSamples(m.ctx)
	if err == nil {
		err = s.ImportLegacyParquet(m.ctx, coldDir)
	}
	if err == nil {
		err = s.checkpointHistoryImport(m.ctx)
	}
	if err == nil {
		s.historyWriteMu.Lock()
		_, err = s.history.ExecContext(m.ctx, `DELETE FROM history_migrations WHERE name='legacy-import-pending'`)
		s.historyWriteMu.Unlock()
	}
	if err != nil {
		slog.Error("historical import paused; live collection continues; original sources retained", "err", err)
		m.update(func(st *HistoryMigrationStatus) {
			st.State = "failed"
			st.LastError = "Historical import paused. Keep the original history files and pre-update backup; check Core logs before retrying."
		})
		return
	}
	m.update(func(st *HistoryMigrationStatus) {
		st.State, st.Phase, st.HistoryComplete = "complete", "complete", true
		st.Activity = ""
		st.CurrentSource = ""
		st.CurrentSourceRowsDone, st.CurrentSourceRowsTotal = 0, 0
		st.IncompleteFromMS, st.IncompleteUntilMS = nil, nil
	})
	slog.Info("historical import complete; all source rows verified")
	if err := s.retireLegacyHistorySources(); err != nil {
		slog.Error("verified history import left legacy sources in place", "err", err)
	}
	s.startSeriesHourBackfill()
}

// SQLite's legacy sample table is frozen after Core selects DuckDB. Each
// verified merge commits its source cursor in the same primary transaction.
// A restart never clears primary rows written by the live collector.
func (s *Store) importSQLiteSamples(ctx context.Context) error {
	var complete int
	if err := s.history.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_migrations WHERE name='sqlite-v1'`).Scan(&complete); err != nil {
		return err
	}
	if complete != 0 {
		var count int64
		// Do not scan the frozen SQLite table on every restart. Older releases
		// have no row cursor; leave their row count unknown instead of scanning.
		if err := s.history.QueryRowContext(ctx, `SELECT COALESCE((SELECT rows_done FROM history_sqlite_progress WHERE source='ts_samples'),0)`).Scan(&count); err != nil {
			return err
		}
		s.historyMigration.update(func(st *HistoryMigrationStatus) { st.RowsDone = count })
		return nil
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ts_samples`).Scan(&count); err != nil {
		return err
	}
	var done, d, m, ts int64
	err := s.history.QueryRowContext(ctx, `SELECT rows_done,driver_id,metric_id,ts_ms FROM history_sqlite_progress WHERE source='ts_samples'`).Scan(&done, &d, &m, &ts)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	s.historyMigration.update(func(st *HistoryMigrationStatus) {
		st.CurrentSource = "state.db/ts_samples"
		st.RowsDone, st.RowsTotal = done, count
		st.CurrentSourceRowsDone, st.CurrentSourceRowsTotal = done, count
	})
	for {
		s.historyActivity("importing")
		if err := s.yieldHistoryImport(ctx); err != nil {
			return err
		}
		q := `SELECT s.driver_id,s.metric_id,s.ts_ms,s.value,d.name,m.name FROM ts_samples s JOIN ts_drivers d ON d.id=s.driver_id JOIN ts_metrics m ON m.id=s.metric_id`
		var args []any
		if done > 0 {
			q += ` WHERE (s.driver_id,s.metric_id,s.ts_ms) > (?,?,?)`
			args = []any{d, m, ts}
		}
		q += ` ORDER BY s.driver_id,s.metric_id,s.ts_ms LIMIT 2048`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		batch := make([]Sample, 0, historyImportRows)
		for rows.Next() {
			var sm Sample
			if err := rows.Scan(&d, &m, &sm.TsMs, &sm.Value, &sm.Driver, &sm.Metric); err != nil {
				rows.Close()
				return err
			}
			ts = sm.TsMs
			batch = append(batch, sm)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		next := done + int64(len(batch))
		if err := s.mergeHistoricalSamples(ctx, batch, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO history_sqlite_progress VALUES ('ts_samples',?,?,?,?) ON CONFLICT(source) DO UPDATE SET rows_done=excluded.rows_done,driver_id=excluded.driver_id,metric_id=excluded.metric_id,ts_ms=excluded.ts_ms`, next, d, m, ts)
			return err
		}); err != nil {
			return err
		}
		done = next
		s.historyMigration.update(func(st *HistoryMigrationStatus) { st.RowsDone, st.CurrentSourceRowsDone = done, done })
		if done%(64*historyImportRows) == 0 {
			if err := s.checkpointHistoryImport(ctx); err != nil {
				return err
			}
		}
	}
	if done != count {
		return fmt.Errorf("SQLite sample count changed: verified %d expected %d", done, count)
	}
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	_, err = s.history.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES ('sqlite-v1')`)
	return err
}

// Let queued live ticks commit before importing another bounded chunk. No
// database or catalog lock is held while the importer yields.
func (s *Store) yieldHistoryImport(ctx context.Context) error {
	waiting := false
	previous := ""
	defer func() {
		if waiting {
			s.historyActivity(previous)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.HistoryWriterStatus().Pending == 0 {
			return nil
		}
		if !waiting {
			previous = s.HistoryMigrationStatus().Activity
			s.historyActivity("waiting_for_live")
			waiting = true
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// A private, chunk-sized staging table bounds the live instance's working set.
// Source-sized sorting and duplicate checks belong to a separate instance.
func (s *Store) mergeHistoricalSamples(ctx context.Context, samples []Sample, receipt func(*sql.Tx) error) error {
	if err := validateHistorySamples(samples); err != nil {
		return err
	}
	if err := s.hydrateIntern(); err != nil {
		return err
	}
	values := make([][]driver.Value, 0, len(samples))
	for _, sm := range samples {
		d, err := s.driverID(sm.Driver)
		if err != nil {
			return err
		}
		m, err := s.metricID(sm.Metric, "")
		if err != nil {
			return err
		}
		values = append(values, []driver.Value{sm.TsMs, d, m, canonicalHistoryFloat(sm.Value)})
	}
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	conn, err := s.history.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TEMP TABLE history_import_source (ts_ms BIGINT NOT NULL,driver_id BIGINT NOT NULL,metric_id BIGINT NOT NULL,value DOUBLE NOT NULL)`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `DROP TABLE IF EXISTS history_import_source`)
	if err := conn.Raw(func(raw any) error {
		app, err := duckdb.NewAppender(nativeHistoryConn(raw), "temp", "main", "history_import_source")
		if err != nil {
			return err
		}
		var appendErr error
		for _, row := range values {
			if appendErr = app.AppendRow(row...); appendErr != nil {
				break
			}
		}
		return errors.Join(appendErr, app.Close())
	}); err != nil {
		return err
	}
	return importHistoryChunkCommit(ctx, conn, 0, int64(len(samples)), receipt)
}
