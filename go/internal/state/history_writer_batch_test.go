package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHistoryWriterBatchesTicksUntilInterval(t *testing.T) {
	s := freshStore(t)
	s.historyWriter.commitInterval = 80 * time.Millisecond
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 1, Driver: "live", Metric: "power", Value: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 2, Driver: "live", Metric: "power", Value: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if st := s.HistoryWriterStatus(); st.Committed != 0 {
		t.Fatalf("committed before interval: %+v", st)
	}
	deadline := time.Now().Add(400 * time.Millisecond)
	for s.HistoryWriterStatus().Committed < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("batched commit did not land: %+v", s.HistoryWriterStatus())
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, err := s.LoadSeries("live", "power", 0, 3, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("series=%v %v", got, err)
	}
}

func TestHistoryWriterFlushCommitsHeldTicks(t *testing.T) {
	s := freshStore(t)
	s.historyWriter.commitInterval = time.Hour
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 1, Driver: "live", Metric: "power", Value: 1}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 2, Driver: "live", Metric: "power", Value: 2}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.HistoryWriterStatus(); st.Committed != 2 || st.Pending != 0 {
		t.Fatalf("flush=%+v", st)
	}
}

func TestHistoryWriterSplitsBatchAfterCommitTimeout(t *testing.T) {
	s := freshStore(t)
	w := s.historyWriter
	w.commitInterval = time.Hour
	w.commitTimeout = 30 * time.Millisecond
	w.commitFn = func(ctx context.Context, batches []historyBatch, ack int64) (historyBatchCommit, error) {
		if len(batches) > 1 {
			<-ctx.Done()
			return historyBatchCommit{}, ctx.Err()
		}
		return s.recordHistoryBatches(ctx, batches, ack)
	}
	for i := 1; i <= 4; i++ {
		if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: int64(i), Driver: "live", Metric: "power", Value: float64(i)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatalf("writer did not recover from a timed-out batch: %+v: %v", s.HistoryWriterStatus(), err)
	}
	st := s.HistoryWriterStatus()
	if st.Accepted != 4 || st.Committed != 4 || st.Pending != 0 || st.LastError != "" {
		t.Fatalf("split recovery status=%+v", st)
	}
	if st.CommitFailures == 0 || st.LastFailureMS == 0 || st.LastFailureError == "" {
		t.Fatalf("recovered commit failures disappeared from diagnostics: %+v", st)
	}
	got, err := s.LoadSeries("live", "power", 0, 5, 0)
	if err != nil || len(got) != 4 {
		t.Fatalf("series=%v %v", got, err)
	}
}

func TestHistoryCommitInterrupted(t *testing.T) {
	if !historyCommitInterrupted(context.DeadlineExceeded) {
		t.Fatal("deadline")
	}
	if !historyCommitInterrupted(context.Canceled) {
		t.Fatal("canceled")
	}
	if historyCommitInterrupted(errors.New("constraint")) {
		t.Fatal("other")
	}
}

func TestLiveMaintenanceKeepsCommittedSamples(t *testing.T) {
	s := freshStore(t)
	s.historyWriter.maintenanceRowsLimit = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 1, Driver: "live", Metric: "power", Value: 1}}, nil); err != nil {
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
	st := s.HistoryWriterStatus()
	if st.MaintenanceError != "" || st.Committed != 1 {
		t.Fatalf("live maintenance=%+v", st)
	}
	got, err := s.LoadSeries("live", "power", 0, 2, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("series=%v %v", got, err)
	}
}
