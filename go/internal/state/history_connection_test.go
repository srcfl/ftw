package state

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

func TestHistoryRotationWaitsForSQLLifetimes(t *testing.T) {
	for _, kind := range []string{"rows", "row", "transaction", "raw", "statement"} {
		t.Run(kind, func(t *testing.T) {
			s := freshStore(t)
			ctx := context.Background()
			var release func()
			switch kind {
			case "rows":
				rows, err := s.history.Query(`SELECT * FROM range(3)`)
				if err != nil {
					t.Fatal(err)
				}
				release = func() {
					var n int64
					if !rows.Next() {
						t.Error("rows lost during attempted rotation")
					}
					if err := rows.Scan(&n); err != nil {
						t.Error(err)
					}
					rows.Close()
				}
			case "row":
				row := s.history.QueryRow(`SELECT 42`)
				release = func() {
					var n int
					if err := row.Scan(&n); err != nil || n != 42 {
						t.Errorf("row=%d %v", n, err)
					}
				}
			case "transaction":
				tx, err := s.history.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				release = func() {
					var n int
					if err := tx.QueryRow(`SELECT 42`).Scan(&n); err != nil || n != 42 {
						t.Errorf("tx=%d %v", n, err)
					}
					tx.Rollback()
				}
			case "statement":
				conn, err := s.history.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				stmt, err := conn.PrepareContext(ctx, `SELECT 42`)
				if err != nil {
					t.Fatal(err)
				}
				release = func() {
					var n int
					if err := stmt.QueryRowContext(ctx).Scan(&n); err != nil || n != 42 {
						t.Errorf("stmt=%d %v", n, err)
					}
					stmt.Close()
					conn.Close()
				}
			case "raw":
				conn, err := s.history.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					done <- conn.Raw(func(raw any) error {
						close(entered)
						<-finish
						_, err := nativeHistoryConn(raw).(driver.ExecerContext).ExecContext(ctx, `SELECT 42`, nil)
						return err
					})
				}()
				<-entered
				release = func() {
					close(finish)
					if err := <-done; err != nil {
						t.Error(err)
					}
					conn.Close()
				}
			}
			native := s.historyConnector.native
			attempt, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			err := s.historyConnector.rotate(attempt)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				release()
				t.Fatalf("rotation ignored %s lease: %v", kind, err)
			}
			if s.historyConnector.native != native {
				release()
				t.Fatal("rotation replaced an active instance")
			}
			// Waiting for the old reader must not block a different live connection.
			if err := s.RecordSamples([]Sample{{TsMs: 1, Driver: "live", Metric: "power", Value: 42}}); err != nil {
				release()
				t.Fatal(err)
			}
			release()
			if err := s.historyConnector.rotate(ctx); err != nil {
				t.Fatal(err)
			}
			if s.historyConnector.native == native {
				t.Fatal("quiescent instance did not rotate")
			}
			if got, err := s.LatestSample("live", "power"); err != nil || got.Value != 42 {
				t.Fatalf("live sample lost: %+v %v", got, err)
			}
		})
	}
}

func TestHistoryConnectHonorsCancellationDuringRotation(t *testing.T) {
	s := freshStore(t)
	s.historyConnector.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := s.historyConnector.Connect(ctx)
	cancel()
	s.historyConnector.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect ignored cancellation: %v", err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryPreparedStatementSurvivesNativeRotation(t *testing.T) {
	s := freshStore(t)
	stmt, err := s.history.Prepare(`SELECT 42`)
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for range 2 {
		if err := s.historyConnector.rotate(context.Background()); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := stmt.QueryRow().Scan(&n); err != nil || n != 42 {
			t.Fatalf("prepared statement=%d %v", n, err)
		}
	}
}

func TestLiveWriterRotatesAfterCommittedRows(t *testing.T) {
	s := freshStore(t)
	s.historyWriter.maintenanceRowsLimit = 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := range 3 {
		if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: int64(i), Driver: "live", Metric: "power", Value: float64(i)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	for s.HistoryWriterStatus().MaintenanceRuns == 0 {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if st := s.HistoryWriterStatus(); st.Committed != 3 || st.MaintenanceError != "" || st.LastMaintenanceMS == 0 {
		t.Fatalf("writer=%+v", st)
	}
	got, err := s.LoadSeries("live", "power", 0, 3, 0)
	if err != nil || len(got) != 3 {
		t.Fatalf("committed rows=%v %v", got, err)
	}
}

func TestLiveWriterRetriesMaintenanceAfterLongReader(t *testing.T) {
	s := freshStore(t)
	s.historyWriter.maintenanceRowsLimit = 1
	s.historyWriter.maintenanceRetryDelay = 0
	reader, err := s.history.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 1, Driver: "live", Metric: "power", Value: 42}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.HistoryWriterStatus(); st.Committed != 1 || st.LastError != "" {
		t.Fatalf("legacy DuckDB reader blocked live SQLite: %+v", st)
	}
	var n int
	if err := reader.QueryRow(`SELECT 42`).Scan(&n); err != nil || n != 42 {
		t.Fatalf("reader interrupted: %d %v", n, err)
	}
	if err := reader.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 2, Driver: "live", Metric: "power", Value: 43}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	for s.HistoryWriterStatus().MaintenanceRuns == 0 {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if st := s.HistoryWriterStatus(); st.MaintenanceError != "" || st.Committed != 2 || st.Rejected != 0 {
		t.Fatalf("maintenance retry=%+v", st)
	}
}
