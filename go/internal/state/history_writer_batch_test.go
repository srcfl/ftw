package state

import (
	"context"
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

func TestHistoryRotatesNativeWhenThresholdIsZero(t *testing.T) {
	prev := historyRotateMinRSS
	historyRotateMinRSS = 0
	t.Cleanup(func() { historyRotateMinRSS = prev })
	s := freshStore(t)
	s.historyWriter.maintenanceRowsLimit = 1
	before := s.historyConnector.native
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
	s.historyConnector.mu.RLock()
	same := s.historyConnector.native == before
	s.historyConnector.mu.RUnlock()
	if same {
		t.Fatal("forced rotation left the native instance in place")
	}
}
