package state

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Regression for the 2026-07-16 incident: the first prune of a months-old
// backlog held one write transaction for 4+ hours (a correlated subquery
// re-scanned history_hot once per bucket) and every concurrent writer failed
// with SQLITE_BUSY. This test prunes a realistic backlog while a writer
// hammers RecordHistory and requires BOTH to succeed.
func TestPruneLargeBacklogWithConcurrentWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("volume test")
	}
	s := freshStore(t)

	// Force many chunks even though the seeded volume is time-compressed.
	origChunk := pruneChunkSpanMS
	pruneChunkSpanMS = 24 * 60 * 60 * 1000
	t.Cleanup(func() { pruneChunkSpanMS = origChunk })

	// 60 days of history at 20 s cadence = 259k rows, all older than
	// HotRetention (30 d) by at least 30 days.
	now := time.Now().UnixMilli()
	start := now - (HotRetention.Milliseconds() + 60*24*60*60*1000)
	pts := make([]HistoryPoint, 0, 259200)
	for ts := start; ts < now-HotRetention.Milliseconds()-24*60*60*1000; ts += 20_000 {
		pts = append(pts, HistoryPoint{TsMs: ts, GridW: 100, JSON: `{"s":1}`})
	}
	if err := s.BulkRecordHistory(pts); err != nil {
		t.Fatal(err)
	}
	seeded := len(pts)
	t.Logf("seeded %d hot rows spanning 60 days", seeded)

	// Concurrent writer: one RecordHistory every 5 ms for the whole prune.
	var writes, writeErrs atomic.Int64
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		i := int64(0)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := s.RecordHistory(HistoryPoint{TsMs: now + i, GridW: 1, JSON: "{}"}); err != nil {
				writeErrs.Add(1)
			} else {
				writes.Add(1)
			}
			i++
			time.Sleep(5 * time.Millisecond)
		}
	}()

	t0 := time.Now()
	err := s.Prune(context.Background())
	elapsed := time.Since(t0)
	close(stop)
	<-writerDone
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	t.Logf("prune took %s; concurrent writes ok=%d failed=%d", elapsed, writes.Load(), writeErrs.Load())

	// The whole point of chunking: no writer may starve out.
	if writeErrs.Load() > 0 {
		t.Fatalf("%d concurrent writes failed during prune (writer starvation)", writeErrs.Load())
	}
	if writes.Load() == 0 {
		t.Fatal("writer never ran — test setup broken")
	}
	// Generous CI bound; the incident shape was hours.
	if elapsed > 60*time.Second {
		t.Fatalf("prune took %s — superlinear behavior is back", elapsed)
	}

	hot, warm, _, err := s.HistoryCounts()
	if err != nil {
		t.Fatal(err)
	}
	// All seeded rows aged out of hot; only the writer's fresh rows remain.
	if hot > int(writes.Load()) {
		t.Fatalf("hot has %d rows after prune, want <= %d fresh writer rows", hot, writes.Load())
	}
	// 60 days at 15-min buckets ≈ 5760 warm rows (some also aged to cold
	// depending on WarmRetention, but seeded data is well within it).
	if warm == 0 {
		t.Fatal("no warm buckets created")
	}
	// Averages must be preserved: every seeded row had grid_w=100.
	var avg float64
	if err := s.db.QueryRow(`SELECT AVG(grid_w) FROM history_warm`).Scan(&avg); err != nil {
		t.Fatal(err)
	}
	if avg != 100 {
		t.Fatalf("warm avg grid_w = %v, want 100", avg)
	}
}

// A bucket must never be aggregated from a partial row set: chunk boundaries
// and the retention cutoff are aligned down to whole buckets, so rows in an
// incomplete tail bucket stay in hot until the bucket has fully aged.
func TestPruneNeverSplitsBuckets(t *testing.T) {
	s := freshStore(t)

	// Rows exactly straddling the aligned cutoff's bucket.
	cutoff := time.Now().UnixMilli() - HotRetention.Milliseconds()
	alignedCutoff := (cutoff / WarmBucketMS) * WarmBucketMS
	inBucketBefore := alignedCutoff - 1    // last row of the fully-aged bucket
	inBucketAfter := alignedCutoff + 1     // first row of the partial bucket
	for _, ts := range []int64{inBucketBefore - 60_000, inBucketBefore, inBucketAfter} {
		if err := s.RecordHistory(HistoryPoint{TsMs: ts, GridW: 50, JSON: "{}"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	var hotLeft int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM history_hot`).Scan(&hotLeft); err != nil {
		t.Fatal(err)
	}
	if hotLeft != 1 {
		t.Fatalf("hot rows left = %d, want exactly the partial-bucket row", hotLeft)
	}
	var maxWarm int64
	if err := s.db.QueryRow(`SELECT MAX(ts_ms) FROM history_warm`).Scan(&maxWarm); err != nil {
		t.Fatal(err)
	}
	if maxWarm >= alignedCutoff {
		t.Fatalf("warm bucket %d at/after aligned cutoff %d — partial bucket was aged", maxWarm, alignedCutoff)
	}
}
