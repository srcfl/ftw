package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryMaintenanceBudgetStaysPending(t *testing.T) {
	previousBudget := historyStageBudget
	historyStageBudget = 30 * time.Millisecond
	t.Cleanup(func() {
		historyStageBudget = previousBudget
		historyStageForTest = nil
	})

	t.Run("stage deadline", func(t *testing.T) {
		s := openBudgetStore(t)
		var seen []string
		historyStageForTest = func(name string, ctx context.Context) (bool, error) {
			seen = append(seen, name)
			if name != "dashboard_rollup" {
				return false, nil
			}
			<-ctx.Done()
			return true, ctx.Err()
		}
		started := time.Now()
		if err := s.MaintainHistory(context.Background(), t.TempDir(), 0, time.Now()); err != nil {
			t.Fatal(err)
		}
		if time.Since(started) > 2*time.Second {
			t.Fatal("a spent stage budget blocked the maintenance turn")
		}
		if len(seen) != 1 || seen[0] != "dashboard_rollup" {
			t.Fatalf("stages after the budget: %v", seen)
		}
		assertPending(t, s)
	})

	t.Run("archive write deadline", func(t *testing.T) {
		s := openBudgetStore(t)
		var seen []string
		historyStageForTest = func(name string, ctx context.Context) (bool, error) {
			seen = append(seen, name)
			if name != "aggregate_archive" {
				return false, nil
			}
			if ctx.Err() != nil {
				t.Errorf("archive stage context already done: %v", ctx.Err())
			}
			return true, context.DeadlineExceeded
		}
		if err := s.MaintainHistory(context.Background(), t.TempDir(), 0, time.Now()); err != nil {
			t.Fatal(err)
		}
		for _, name := range seen {
			if name == "sample_archive" || name == "legacy_compaction" {
				t.Fatalf("kept going after the archive budget: %v", seen)
			}
		}
		assertPending(t, s)
	})

	t.Run("stuck dashboard bucket", func(t *testing.T) {
		s := openBudgetStore(t)
		historyStageForTest = func(name string, ctx context.Context) (bool, error) {
			if name != "dashboard_rollup" {
				return false, nil
			}
			if ctx.Err() != nil {
				t.Fatalf("stage budget already spent: %v", ctx.Err())
			}
			return true, fmt.Errorf("dashboard bucket 1 cannot commit within its write budget: %w", context.DeadlineExceeded)
		}
		err := s.MaintainHistory(context.Background(), t.TempDir(), 0, time.Now())
		if err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		st := s.HistoryMaintenanceStatus()
		if st.State != "failed" || st.Failures != 1 || st.LastError == "" {
			t.Fatalf("stuck bucket hidden as a pause: %+v", st)
		}
	})

	t.Run("caller cancelled", func(t *testing.T) {
		s := openBudgetStore(t)
		parent, cancel := context.WithCancel(context.Background())
		historyStageForTest = func(name string, ctx context.Context) (bool, error) {
			if name != "dashboard_rollup" {
				return false, nil
			}
			cancel()
			<-ctx.Done()
			return true, ctx.Err()
		}
		err := s.MaintainHistory(parent, t.TempDir(), 0, time.Now())
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if st := s.HistoryMaintenanceStatus(); st.State == "pending" {
			t.Fatalf("shutdown stored as pending work: %+v", st)
		}
	})
}

func TestArchiveBudgetCancelsAndLiveHistoryCommits(t *testing.T) {
	previous := historyArchiveTurn
	historyArchiveTurn = 80 * time.Millisecond
	t.Cleanup(func() {
		historyArchiveTurn = previous
		historyStageForTest = nil
	})
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	historyStageForTest = func(name string, ctx context.Context) (bool, error) {
		if name != "sample_archive" {
			return false, nil
		}
		close(started)
		<-ctx.Done()
		return true, ctx.Err()
	}
	errc := make(chan error, 1)
	go func() {
		<-started
		now := time.Now().UnixMilli()
		for i := 0; i < 8; i++ {
			if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: now + int64(i), Value: float64(i)}}, nil); err != nil {
				errc <- err
				return
			}
		}
		flush, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		errc <- s.FlushHistory(flush)
	}()
	maintainStarted := time.Now()
	if err := s.MaintainHistory(context.Background(), t.TempDir(), 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if time.Since(maintainStarted) > 2*time.Second {
		t.Fatal("sample archive had no cancellable budget")
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	writer := s.HistoryWriterStatus()
	if writer.Rejected != 0 || writer.Committed != 8 || writer.Pending != 0 {
		t.Fatalf("live history stalled behind maintenance: %+v", writer)
	}
	assertPending(t, s)
	var raw int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&raw); err != nil || raw != 0 {
		t.Fatalf("live ticks stored as raw samples: %d %v", raw, err)
	}
}

func TestRepeatedPollsStayInBuckets(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	for i := 0; i < 200; i++ {
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "grid_w", TsMs: base + int64(i)*100, Value: float64(i)}}, nil); err != nil {
			t.Fatal(err)
		}
		if (i+1)%16 == 0 {
			if err := s.FlushHistory(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	var raw, buckets int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets`).Scan(&buckets); err != nil {
		t.Fatal(err)
	}
	if raw != 0 || buckets < 2 || buckets > 6 {
		t.Fatalf("200 polls became raw=%d buckets=%d", raw, buckets)
	}
}

func openBudgetStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func assertPending(t *testing.T, s *Store) {
	t.Helper()
	st := s.HistoryMaintenanceStatus()
	if st.State != "pending" || st.Failures != 0 || st.LastError != "" {
		t.Fatalf("status = %+v, want pending with no failure", st)
	}
}
