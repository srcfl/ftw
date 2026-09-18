package state

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStopHistoryCancelsBackfillAndDrainsMultipleSlowBatches(t *testing.T) {
	s := freshStore(t)
	w := s.historyWriter
	w.commitInterval = time.Hour
	w.commitMaxTicks = 1
	// Total drain exceeds one commit budget, while each real SQLite commit
	// has room to run under a loaded CI host. This is a lifecycle test;
	// target IO latency has separate opt-in admission fixtures.
	w.commitTimeout = 2 * time.Second
	w.shutdownTimeout = 10 * time.Second
	backfill, cancel := context.WithCancel(context.Background())
	s.seriesHourCancel = cancel
	s.seriesHourWG.Add(1)
	go func() { defer s.seriesHourWG.Done(); <-backfill.Done() }()
	w.commitFn = func(ctx context.Context, batches []historyBatch, ack int64) (historyBatchCommit, error) {
		select {
		case <-backfill.Done():
		case <-ctx.Done():
			return historyBatchCommit{}, ctx.Err()
		}
		timer := time.NewTimer(600 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return historyBatchCommit{}, ctx.Err()
		}
		return s.recordHistoryBatches(ctx, batches, ack)
	}
	for i := 1; i <= 4; i++ {
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "live", Metric: "power", TsMs: int64(i), Value: float64(i)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.StopHistory(); err != nil {
		t.Fatal(err)
	}
	st := s.HistoryWriterStatus()
	if st.Accepted != 4 || st.Committed != 4 || st.Pending != 0 || !st.Stopping {
		t.Fatalf("incomplete shutdown: %+v", st)
	}
	got, err := s.LoadSeries("live", "power", 0, 5, 0)
	if err != nil || len(got) != 4 {
		t.Fatalf("accepted samples missing: %v %v", got, err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "live", Metric: "power", TsMs: 5, Value: 5}}, nil); err == nil {
		t.Fatal("accepted data after shutdown")
	}
	if err := s.StopHistory(); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

func TestStopHistoryReportsUncommittedTicksAfterBudget(t *testing.T) {
	s := freshStore(t)
	w := s.historyWriter
	w.commitInterval = time.Hour
	w.commitTimeout = 20 * time.Millisecond
	w.shutdownTimeout = 50 * time.Millisecond
	w.commitFn = func(ctx context.Context, _ []historyBatch, _ int64) (historyBatchCommit, error) {
		<-ctx.Done()
		return historyBatchCommit{}, ctx.Err()
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "live", Metric: "power", TsMs: 1, Value: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.StopHistory(); err == nil || !strings.Contains(err.Error(), "1 ticks uncommitted") {
		t.Fatalf("shutdown result: %v", err)
	}
	if st := s.HistoryWriterStatus(); st.Committed != 0 || st.Pending != 1 {
		t.Fatalf("lost failure state: %+v", st)
	}
	if err := s.Close(); err == nil {
		t.Fatal("Close hid the incomplete drain")
	}
}
