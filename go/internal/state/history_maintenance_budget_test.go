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
