package state

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestDailyCostBreakdownContextCancelsWaitingRead(t *testing.T) {
	for _, database := range []string{"prices", "history"} {
		t.Run(database, func(t *testing.T) {
			s := freshStore(t)
			db := s.cache
			if database == "history" {
				db = s.history
			}
			// Hold the whole pool, so the request must wait for an actual
			// database connection rather than a timing-dependent SQLite lock.
			db.SetMaxOpenConns(1)
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			waits := db.Stats().WaitCount
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := s.DailyCostBreakdownContext(ctx, 0, 3_600_000, "SE3", ExportPricing{})
				done <- err
			}()
			waitForCostDBWait(t, db, waits)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled read = %v, want context.Canceled", err)
				}
			case <-time.After(time.Second):
				// Release the blocked worker before failing, so the store can
				// close even when this test runs against the old implementation.
				_ = conn.Close()
				<-done
				t.Fatal("canceled cost read still waits for a database connection")
			}
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := s.DailyCostBreakdownContext(context.Background(), 0, 3_600_000, "SE3", ExportPricing{}); err != nil {
				t.Fatalf("next read after cancellation: %v", err)
			}
		})
	}
}

func waitForCostDBWait(t *testing.T, db *sql.DB, before int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for db.Stats().WaitCount == before {
		if time.Now().After(deadline) {
			t.Fatal("cost query did not wait for the held database connection")
		}
		time.Sleep(time.Millisecond)
	}
}
