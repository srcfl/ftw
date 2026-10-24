package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestArchiveWriteFailureReportsPhaseAndPreservesRows(t *testing.T) {
	for _, phase := range []string{"connection", "prepare", "archive_view_lock", "writer_lock", "write"} {
		t.Run(phase, func(t *testing.T) {
			s := freshStore(t)
			if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: 1000, Value: 1}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.history.Exec(`CREATE TABLE archive_timing_probe(id INTEGER PRIMARY KEY); INSERT INTO archive_timing_probe VALUES(1)`); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			var release func()
			switch phase {
			case "connection":
				s.history.SetMaxOpenConns(1)
				conn, err := s.history.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				release = func() { _ = conn.Close() }
			case "archive_view_lock":
				s.archiveViewMu.Lock()
				release = s.archiveViewMu.Unlock
			case "writer_lock":
				s.historyWriteMu.Lock()
				release = s.historyWriteMu.Unlock
			}
			var prepare func(context.Context, *sql.Tx) error
			if phase == "prepare" {
				prepare = func(ctx context.Context, tx *sql.Tx) error {
					var count int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM archive_timing_probe`).Scan(&count); err != nil {
						return err
					}
					<-ctx.Done()
					return ctx.Err()
				}
			}
			err := s.tryArchiveBatch(ctx, prepare, func(ctx context.Context, tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx, `INSERT INTO archive_timing_probe VALUES(2)`); err != nil {
					return err
				}
				if phase == "write" {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			})
			if release != nil {
				release()
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("archive failure lost its deadline: %v", err)
			}
			for _, want := range []string{"phase=" + phase, "elapsed_ms=", "step_ms=", phase + "_max_ms=", phase + "_steps=1"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "archive_timing_probe") {
				t.Fatal("archive timing exposed SQL contents")
			}
			var count int
			if err := s.history.QueryRow(`SELECT COUNT(*) FROM archive_timing_probe`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("failed attempt changed committed rows: count=%d err=%v", count, err)
			}
			if err := s.tryArchiveBatch(context.Background(), nil, func(ctx context.Context, tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO archive_timing_probe VALUES(2)`)
				return err
			}); err != nil {
				t.Fatal("retry:", err)
			}
			if err := s.history.QueryRow(`SELECT COUNT(*) FROM archive_timing_probe`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("retry lost or duplicated rows: count=%d err=%v", count, err)
			}
		})
	}
}
