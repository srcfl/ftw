package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestCheckpointWALDoesNotWaitForReaders(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveConfigValues(map[string]string{"checkpoint-test": "before"}); err != nil {
		t.Fatal(err)
	}
	reader, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var n int
	if err := reader.QueryRow("SELECT COUNT(*) FROM config").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfigValues(map[string]string{"checkpoint-test": "after"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.CheckpointWAL(); close(done) }()
	defer func() { reader.Rollback(); <-done }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("background checkpoint waits for an old reader while excluding live writers")
	}
	// The old reader is still open. Durable user goals must remain writable.
	if err := s.SaveConfigValues(map[string]string{"checkpoint-test": "goal"}); err != nil {
		t.Fatal(err)
	}
}

func TestForecastPruneReadDoesNotBlockDurableGoals(t *testing.T) {
	reading, release := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	defer released.Do(func() { close(release) })
	fn := fmt.Sprintf("test_forecast_prune_read_%d", time.Now().UnixNano())
	if err := sqlite.RegisterScalarFunction(fn, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		entered.Do(func() { close(reading); <-release })
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	s := openForecastArchive(t)
	for i := 1; i <= 130; i++ {
		if _, err := s.db.Exec(`INSERT INTO forecast_observations VALUES(?,?,?,'old','{}')`, i, i+1, i+2); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		n, err := s.pruneForecastRows(ctx, "forecast_observations", `SELECT rowid FROM forecast_observations WHERE `+fn+`(rowid)>0 LIMIT 64`)
		if err == nil && n != 130 {
			err = fmt.Errorf("deleted %d rows, want 130", n)
		}
		done <- err
	}()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	goal := make(chan error, 1)
	go func() { goal <- s.SaveConfigValues(map[string]string{"test-goal": "80"}) }()
	select {
	case err := <-goal:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		released.Do(func() { close(release) })
		<-done
		<-goal
		t.Fatal("forecast budget scan held the state writer lock")
	}
	// The config commit invalidates the pruning snapshot. The retry must
	// reselect, then finish all three delete batches without losing counts.
	released.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if value, ok := s.LoadConfig("test-goal"); !ok || value != "80" {
		t.Fatal("durable goal changed")
	}
}

func TestArchiveSingleRowDeadlineRetainsCurrentBatch(t *testing.T) {
	s := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.RecordSamples([]Sample{{Driver: "test", Metric: "power", TsMs: 1, Value: 42}}); err != nil {
		t.Fatal(err)
	}
	d, err := s.driverID("test")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.metricID("power", "")
	if err != nil {
		t.Fatal(err)
	}
	// One long reader outlasts the archive's write budget. The verified row
	// must stay in this job and retry after the reader releases the view.
	s.archiveViewMu.RLock()
	locked := true
	defer func() {
		if locked {
			s.archiveViewMu.RUnlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		n, err := s.pruneArchivedSamples(ctx, []resolvedSample{{dID: d, mID: m, ts: 1, v: 42}})
		if err == nil && n != 1 {
			err = sql.ErrNoRows
		}
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("archive abandoned its verified row after one write deadline: %v", err)
	case <-time.After(archiveWriteTimeout + 150*time.Millisecond):
	}
	s.archiveViewMu.RUnlock()
	locked = false
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
