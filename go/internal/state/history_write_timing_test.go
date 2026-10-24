package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestHistoryWriteFailureReportsSQLTimingsAndRetries(t *testing.T) {
	for _, phase := range []string{"read_bucket", "write_bucket"} {
		t.Run(phase, func(t *testing.T) {
			s := freshStore(t)
			if err := s.EnableHistoryAggregation(); err != nil {
				t.Fatal(err)
			}
			base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
			if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "grid_w", TsMs: base, Value: 10}}); err != nil {
				t.Fatal(err)
			}
			if err := s.history.Close(); err != nil {
				t.Fatal(err)
			}
			d := &slowHistoryRollbackDriver{blocked: make(chan struct{})}
			if phase == "read_bucket" {
				d.queryNeedle = "SELECT seen_ms FROM ts_buckets"
			} else {
				d.execNeedle = "INSERT INTO ts_buckets"
			}
			name := fmt.Sprintf("history-write-timing-%d", time.Now().UnixNano())
			sql.Register(name, d)
			var err error
			s.history, err = sql.Open(name, s.historyPath+strings.Replace(sqlitePragmas, "synchronous(NORMAL)", "synchronous(FULL)", 1))
			if err != nil {
				t.Fatal(err)
			}
			batch := historyBatch{id: "timing", hash: "payload", payload: historyPayload{Samples: []Sample{{Driver: "meter", Metric: "grid_w", TsMs: base + 1000, Value: 20}}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d.armed.Store(true)
			done := make(chan error, 1)
			go func() {
				_, err := s.recordHistoryBatches(ctx, []historyBatch{batch}, 0)
				done <- err
			}()
			select {
			case <-d.blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("write did not reach selected SQL operation")
			}
			cancel()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("cancelled write did not finish")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("timings lost the original error: %v", err)
			}
			for _, want := range []string{"history tick: samples:", "phase=" + phase, "elapsed_ms=", "step_ms=", "prepare_ms=", "writer_lock_ms=", "connection_ms=", "begin_ms=", "tick_ms=", phase + "_max_ms=", phase + "_steps=1"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("missing %q: %v", want, err)
				}
			}
			for _, private := range []string{"timing", "payload", "grid_w", fmt.Sprint(base + 1000)} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("failure timings exposed a sample or batch value: %v", err)
				}
			}
			for i := 0; i < 2; i++ {
				if _, err := s.recordHistoryBatches(context.Background(), []historyBatch{batch}, 0); err != nil {
					t.Fatal("retry:", err)
				}
			}
			var n int
			var sum float64
			if err := s.history.QueryRow(`SELECT n,sum_value FROM ts_buckets WHERE resolution_ms=? AND start_ms=?`, HistoryResolutionMS, base).Scan(&n, &sum); err != nil || n != 1 || sum != 20 {
				t.Fatalf("retry lost or duplicated samples: n=%d sum=%v err=%v", n, sum, err)
			}
		})
	}
}

func TestHistoryWriteTimingSeparatesRepeatedStepMaximumFromTotal(t *testing.T) {
	timing := newHistoryWriteTiming("other")
	timing.add("read_bucket", 2*time.Millisecond)
	timing.add("read_bucket", 7*time.Millisecond)
	err := timing.failure(context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("timing lost cancellation: %v", err)
	}
	for _, want := range []string{"read_bucket_ms=9", "read_bucket_max_ms=7", "read_bucket_steps=2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("repeated step timing missing %q: %v", want, err)
		}
	}
}

func TestHistoryWriteFailureReportsWriterLockWait(t *testing.T) {
	s := freshStore(t)
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "grid_w", TsMs: 1000, Value: 10}}); err != nil {
		t.Fatal(err)
	}
	s.historyWriteMu.Lock()
	defer s.historyWriteMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	batch := historyBatch{id: "locked", hash: "payload", payload: historyPayload{Samples: []Sample{{Driver: "meter", Metric: "grid_w", TsMs: 2000, Value: 20}}}}
	_, err := s.recordHistoryBatches(ctx, []historyBatch{batch}, 0)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "history writer lock:") || !strings.Contains(err.Error(), "writer_lock_ms=") {
		t.Fatalf("missing lock wait timing or deadline: %v", err)
	}
	var receipts int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_receipts WHERE batch_id='locked'`).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("failed lock wait wrote a receipt: %d %v", receipts, err)
	}
}
