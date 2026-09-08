package state

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryPrimaryAndRetryReceipt(t *testing.T) {
	s := freshStore(t)
	p := HistoryPoint{TsMs: 1000, GridW: 42, JSON: `{"source":"meter"}`}
	samples := []Sample{{Driver: "meter", Metric: "grid_w", TsMs: 1000, Value: 42, Unit: "W"}}
	seq, err := s.recordHistoryBatch(context.Background(), "batch-a", "hash-a", &p, samples, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.recordHistoryBatch(context.Background(), "batch-a", "hash-a", &p, samples, nil, 0)
	if err != nil || seq != again || seq == 0 {
		t.Fatalf("retry seq=%d/%d err=%v", seq, again, err)
	}
	if _, err := s.recordHistoryBatch(context.Background(), "batch-a", "hash-b", &p, samples, nil, 0); err == nil {
		t.Fatal("accepted changed payload with an existing receipt")
	}
	p.GridW = 84
	samples[0].Value = 84
	if err := s.RecordTick(p, samples); err != nil {
		t.Fatal(err)
	}
	h, err := s.LoadHistory(1000, 1000, 0)
	if err != nil || len(h) != 1 || h[0].GridW != 84 {
		t.Fatalf("history last-write semantics: %+v %v", h, err)
	}
	v, err := s.LoadSeries("meter", "grid_w", 1000, 1000, 0)
	if err != nil || len(v) != 1 || v[0].Value != 42 {
		t.Fatalf("sample first-write semantics: %+v %v", v, err)
	}
	for _, table := range []string{"history_hot", "ts_samples"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("live write reached SQLite %s: %d %v", table, n, err)
		}
	}
}

func TestHistoryQueueDoesNotWaitOnDiskAndRejectsOverflow(t *testing.T) {
	s := freshStore(t)
	s.historyWriteMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.historyWriteMu.Unlock()
		}
	}()
	samples := []Sample{{Driver: "meter", Metric: "power", TsMs: 1, Value: 17}}
	start := time.Now()
	for i := 0; i < historyQueueTicks; i++ {
		samples[0].TsMs = int64(i + 1)
		if err := s.EnqueueTelemetryTick(nil, samples, nil); err != nil {
			t.Fatal(err)
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("queue admission waited on disk")
	}
	if err := s.EnqueueTelemetryTick(nil, samples, nil); err == nil {
		t.Fatal("overflow accepted")
	}
	samples[0].Value = 999 // caller memory may change immediately after admission
	status := s.HistoryWriterStatus()
	if status.Committed != 0 || status.Accepted != 64 || status.Rejected != 1 || status.Pending != 64 {
		t.Fatalf("false commit or admission counts: %+v", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if !errors.Is(s.FlushHistory(ctx), context.DeadlineExceeded) {
		t.Fatal("flush claimed a blocked write was durable")
	}
	s.historyWriteMu.Unlock()
	locked = false
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	if err := s.FlushHistory(ctx2); err != nil {
		t.Fatal(err)
	}
	v, err := s.LoadSeries("meter", "power", 0, 100, 0)
	if err != nil || len(v) != 64 {
		t.Fatalf("drained samples=%d err=%v", len(v), err)
	}
	for _, sm := range v {
		if sm.Value != 17 {
			t.Fatal("queued payload changed with caller memory")
		}
	}
	var receipts int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_receipts`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("serial writer retained %d receipts: %v", receipts, err)
	}
}

func TestHistoryReceiptRetirementPreservesUncertainCommit(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	first, err := s.recordHistoryBatch(ctx, "first", "first-hash", &HistoryPoint{TsMs: 1}, nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.recordHistoryBatch(ctx, "second", "second-hash", &HistoryPoint{TsMs: 2}, nil, nil, first)
	if err != nil {
		t.Fatal(err)
	}
	// The writer has not observed the second result. Retrying the same payload
	// must return its original sequence even though the first receipt is gone.
	again, err := s.recordHistoryBatch(ctx, "second", "second-hash", &HistoryPoint{TsMs: 2}, nil, nil, first)
	if err != nil || again != second {
		t.Fatalf("uncertain commit retry=%d, want %d: %v", again, second, err)
	}
	if _, err := s.recordHistoryBatch(ctx, "second", "changed", nil, nil, nil, first); err == nil {
		t.Fatal("uncertain receipt accepted a changed payload")
	}
	var count int
	var batch string
	if err := s.history.QueryRow(`SELECT COUNT(*),MIN(batch_id) FROM history_receipts`).Scan(&count, &batch); err != nil || count != 1 || batch != "second" {
		t.Fatalf("receipts=%d %q: %v", count, batch, err)
	}
	// Invalid acknowledgement rolls back both the new data and its receipt.
	if _, err := s.recordHistoryBatch(ctx, "invalid", "invalid-hash", &HistoryPoint{TsMs: 3}, nil, nil, math.MaxInt64); err == nil {
		t.Fatal("accepted an acknowledgement beyond the current commit")
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_hot WHERE ts_ms=3`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed batch left data behind: %d %v", count, err)
	}
	if _, err := s.recordHistoryBatch(ctx, "second", "second-hash", nil, nil, nil, first); err != nil {
		t.Fatalf("failed batch removed uncertain receipt: %v", err)
	}
}

func TestHistoryWriterRetriesFailedTransaction(t *testing.T) {
	s := freshStore(t)
	if _, err := s.history.Exec(`ALTER TABLE history_hot RENAME TO history_unavailable`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(&HistoryPoint{TsMs: 1, GridW: 42}, nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.HistoryWriterStatus().LastError == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := s.HistoryWriterStatus()
	if status.LastError == "" || status.Pending != 1 || status.Committed != 0 {
		t.Fatalf("failed write was not retained: %+v", status)
	}
	if _, err := s.history.Exec(`ALTER TABLE history_unavailable RENAME TO history_hot`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_receipts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("retry receipts=%d %v", n, err)
	}
}

func TestHistoryMissingOrUnboundPrimaryFails(t *testing.T) {
	for _, kind := range []string{"missing", "unbound", "incomplete"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.RecordHistory(HistoryPoint{TsMs: 1}); err != nil {
				t.Fatal(err)
			}
			if kind == "unbound" {
				if _, err := s.db.Exec(`DELETE FROM config WHERE key LIKE 'history_%'`); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "incomplete" {
				if _, err := s.history.Exec(`DELETE FROM history_migrations WHERE name='sqlite-v1'`); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				if err := os.Remove(historyDatabasePath(path)); err != nil {
					t.Fatal(err)
				}
			}
			if reopened, err := Open(path); err == nil {
				reopened.Close()
				t.Fatal("silently accepted missing or unbound history")
			}
		})
	}
}

func TestOfflineBackupIncludesDuckDBCorrectionsAndRestoresBesideOldPrimary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: 1, GridW: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: 100, GridW: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: 1, GridW: 20}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := OpenBackupSource(path)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "export.db")
	if _, err := backup.db.Exec(`VACUUM INTO '` + dst + `'`); err != nil {
		t.Fatal(err)
	}
	if err := backup.exportHistoryToSQLite(dst); err != nil {
		t.Fatal(err)
	}
	backup.Close()
	// An older Core sees the complete export, including corrections to old rows.
	old, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	var value float64
	if err := old.QueryRow(`SELECT grid_w FROM history_hot WHERE ts_ms=1`).Scan(&value); err != nil || value != 20 {
		t.Fatalf("old reader got %v %v", value, err)
	}
	// Simulate further writes by the older Core before another upgrade.
	if _, err := old.Exec(`INSERT INTO history_hot(ts_ms,grid_w,json) VALUES (200,200,'{}')`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	data, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	h, err := restored.LoadHistory(0, 300, 0)
	if err != nil || len(h) != 3 || h[0].GridW != 20 || h[2].GridW != 200 {
		t.Fatalf("restore/old/new lost history: %+v %v", h, err)
	}
	prior, err := filepath.Glob(historyDatabasePath(path) + ".before-restore-*")
	if err != nil || len(prior) == 0 {
		t.Fatal("restore did not preserve previous DuckDB")
	}
}

func TestHistoryRejectsNonFiniteAndCanonicalizesZero(t *testing.T) {
	s := freshStore(t)
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := s.RecordSamples([]Sample{{Driver: "d", Metric: "m", TsMs: 1, Value: v}}); err == nil {
			t.Fatal("accepted non-finite sample")
		}
		if err := s.RecordHistory(HistoryPoint{TsMs: 1, GridW: v}); err == nil {
			t.Fatal("accepted non-finite history")
		}
		if err := s.BulkRecordHistory([]HistoryPoint{{TsMs: 1, GridW: 1}, {TsMs: 2, GridW: v}}); err == nil {
			t.Fatal("accepted non-finite backfill")
		}
	}
	if err := s.RecordTick(HistoryPoint{TsMs: 1, GridW: math.Copysign(0, -1)}, []Sample{{Driver: "d", Metric: "m", TsMs: 1, Value: math.Copysign(0, -1)}}); err != nil {
		t.Fatal(err)
	}
	v, err := s.LatestSample("d", "m")
	if err != nil || math.Float64bits(v.Value) != 0 {
		t.Fatalf("zero was not canonical: %v %v", v, err)
	}
	if err := s.BackupToCompressed(filepath.Join(t.TempDir(), "zero.db.gz")); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryQueryCancellationAndRetention(t *testing.T) {
	s := freshStore(t)
	now := time.Now().UTC()
	if err := s.RecordSamples([]Sample{{Driver: "d", Metric: "m", TsMs: now.AddDate(0, 0, -40).UnixMilli(), Value: 1}, {Driver: "d", Metric: "m", TsMs: now.UnixMilli(), Value: 2}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.LoadHistoryContext(ctx, 0, now.UnixMilli(), 100); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.LoadSeriesBucketsOrRawContext(ctx, "d", "m", 0, now.UnixMilli(), 100); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.PruneHistorySamples(context.Background(), 0, now); err != nil {
		t.Fatal(err)
	}
	all, err := s.LoadSeries("d", "m", 0, now.UnixMilli(), 0)
	if err != nil || len(all) != 2 {
		t.Fatal("unlimited retention lost history")
	}
	if err := s.PruneHistorySamples(context.Background(), 30, now); err != nil {
		t.Fatal(err)
	}
	all, err = s.LoadSeries("d", "m", 0, now.UnixMilli(), 0)
	if err != nil || len(all) != 1 || all[0].Value != 2 {
		t.Fatalf("retention: %+v %v", all, err)
	}
}

func TestBulkHistoryDuplicateLastValueWins(t *testing.T) {
	s := freshStore(t)
	if err := s.BulkRecordHistory([]HistoryPoint{{TsMs: 1, GridW: 10}, {TsMs: 1, GridW: 20}}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadHistory(1, 1, 0)
	if err != nil || len(rows) != 1 || rows[0].GridW != 20 {
		t.Fatalf("bulk duplicate changed precedence: %+v %v", rows, err)
	}
}
