package state

import (
	"context"
	"testing"
	"time"
)

func TestBackupPausesMaintenanceWithoutStoppingWriter(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { historyStageForTest = nil })
	entered := make(chan struct{})
	historyStageForTest = func(name string, ctx context.Context) (bool, error) {
		if name != "plain_buckets" {
			return false, nil
		}
		close(entered)
		<-ctx.Done()
		return true, ctx.Err()
	}
	done := make(chan error, 1)
	go func() { done <- s.MaintainPlainHistory(context.Background(), time.Now()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resume, err := s.PauseHistoryMaintenance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("backup pause became a storage failure:", err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: time.Now().UnixMilli(), Value: 42}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainPlainHistory(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if st := s.HistoryMaintenanceStatus(); st.State != "paused" || st.Failures != 0 {
		t.Fatalf("pause=%+v", st)
	}
	if st := s.HistoryWriterStatus(); st.Committed != 1 || st.Rejected != 0 {
		t.Fatalf("writer=%+v", st)
	}
	resume()
}
