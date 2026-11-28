package state

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStateSnapshotLiveHistoryLatency times live history commits, with their
// real five-second deadline, while the recovery snapshot copies state.db.
// It needs slow storage: on a laptop SSD every commit ends in milliseconds,
// with or without pacing. Point FTW_SNAPSHOT_LATENCY_DIR at a directory on a
// throttled block device. Run FTW_SNAPSHOT_LATENCY_PHASE=prepare first, drop
// the page cache (remount), throttle the device, then run phase "measure".
// One way: in a privileged arm64 Linux container, mount ext4 on a loop
// device and write "MAJ:MIN rbps=41943040 wbps=8388608 riops=1500 wiops=400"
// to the container's cgroup io.max, about the speed of an SD card.
// FTW_SNAPSHOT_LATENCY_LIMIT fails the measure phase when a commit or config
// write that starts during the snapshot takes longer than the limit.
func TestStateSnapshotLiveHistoryLatency(t *testing.T) {
	dir := os.Getenv("FTW_SNAPSHOT_LATENCY_DIR")
	if dir == "" {
		t.Skip("set FTW_SNAPSHOT_LATENCY_DIR to a directory on slow storage")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	switch phase := os.Getenv("FTW_SNAPSHOT_LATENCY_PHASE"); phase {
	case "prepare":
		scale := 1.0
		if v := os.Getenv("FTW_SNAPSHOT_LATENCY_SCALE"); v != "" {
			var err error
			if scale, err = strconv.ParseFloat(v, 64); err != nil || scale <= 0 {
				t.Fatalf("FTW_SNAPSHOT_LATENCY_SCALE=%q", v)
			}
		}
		prepareSnapshotLatency(t, path, scale)
	case "measure", "":
		measureSnapshotLatency(t, path)
	default:
		t.Fatalf("FTW_SNAPSHOT_LATENCY_PHASE=%q, want prepare or measure", phase)
	}
}

const (
	latencyDrivers = 3
	latencyMetrics = 20
)

func latencyTick(nowMS int64, i int) (*HistoryPoint, []Sample) {
	samples := make([]Sample, 0, latencyDrivers*latencyMetrics)
	for d := range latencyDrivers {
		for m := range latencyMetrics {
			samples = append(samples, Sample{Driver: fmt.Sprintf("driver%d", d), Metric: fmt.Sprintf("metric_%02d_w", m), TsMs: nowMS, Value: float64(i%997 + d + m)})
		}
	}
	point := &HistoryPoint{TsMs: nowMS, GridW: float64(i % 3000), PVW: -1200, BatW: 300, LoadW: 1800, BatSoC: 0.5, JSON: `{"grid_w":1,"pv_w":-1200,"bat_w":300,"load_w":1800}`}
	return point, samples
}

// prepareSnapshotLatency builds a state.db whose recovery snapshot copies
// about 130 MB at scale 1: the forecast tables halfway to their retention
// budgets plus events and the control journal. history.db gets five days
// of 10-second buckets for 60 series, so live upserts walk a real B-tree.
func prepareSnapshotLatency(t *testing.T, path string, scale float64) {
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1554))
	blob := func(n int) []byte {
		b := make([]byte, n)
		rng.Read(b)
		return b
	}
	text := func(n int) string { return hex.EncodeToString(blob(n / 2)) }
	rows := func(n int) int { return max(1, int(float64(n)*scale)) }
	fill := func(n int, stmt string, args func(int) []any) {
		t.Helper()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		insert, err := tx.PrepareContext(ctx, stmt)
		if err != nil {
			t.Fatal(err)
		}
		defer insert.Close()
		for i := range n {
			if _, err := insert.ExecContext(ctx, args(i)...); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	const day = int64(24 * time.Hour / time.Millisecond)
	start := time.Now().UnixMilli() - 30*day
	fill(rows(768), `INSERT INTO forecast_issues VALUES(?,?,?,?,?)`, func(i int) []any {
		return []any{fmt.Sprintf("issue-%06d", i), start + int64(i)*900_000, start + int64(i)*900_000 + 1, "v1", blob(64 << 10)}
	})
	fill(rows(256), `INSERT INTO forecast_model_states VALUES(?,?,?,?)`, func(i int) []any {
		return []any{fmt.Sprintf("state-%06d", i), 256 << 10, start + int64(i)*3_600_000, blob(128 << 10)}
	})
	fill(rows(65536), `INSERT INTO forecast_errors VALUES(?,?,?,?,?,?,?,?,?,?)`, func(i int) []any {
		at := start + int64(i)*60_000
		return []any{fmt.Sprintf("series-%d", i%8), "v1", i % 48, at, at + 900_000, at, at, fmt.Sprintf("issue-%06d", i%768), at + 900_000, text(512)}
	})
	fill(rows(8192), `INSERT INTO forecast_observations VALUES(?,?,?,?,?)`, func(i int) []any {
		at := start + int64(i)*900_000
		return []any{at, at + 900_000, at + 900_000, "v1", text(1024)}
	})
	fill(rows(50000), `INSERT INTO events VALUES(?,?)`, func(i int) []any {
		return []any{start + int64(i)*1000, text(100)}
	})
	fill(rows(40000), `INSERT INTO control_journal(subject,driver,kind,status,reason,start_ms,end_ms,updated_ms,first_json,last_json) VALUES(?,?,?,?,?,?,?,?,?,?)`, func(i int) []any {
		at := start + int64(i)*60_000
		return []any{fmt.Sprintf("driver%d", i%3), fmt.Sprintf("driver%d", i%3), "battery", "following", "", at, at + 60_000, at + 60_000, text(80), text(80)}
	})

	// Intern the live series, then add older 10-second buckets beneath them.
	now := time.Now().UnixMilli()
	point, samples := latencyTick(now, 0)
	if err := s.EnqueueTelemetryTick(point, samples, nil); err != nil {
		t.Fatal(err)
	}
	flushCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.FlushHistory(flushCtx); err != nil {
		t.Fatal(err)
	}
	buckets := int64(float64(5*day/HistoryResolutionMS) * scale)
	newest := bucketStart(now, HistoryResolutionMS) - HistoryResolutionMS
	tx, err := s.history.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM k WHERE i+1<?)
 INSERT INTO ts_buckets(driver_id,metric_id,start_ms,resolution_ms,first_ms,last_ms,n,sum_value,min_value,max_value,last_value,seen_ms)
 SELECT d.id,m.id,?-k.i*?,?,?-k.i*?,?-k.i*?+8000,5,5.0,1.0,1.0,1.0,X'0000000200040006'
 FROM ts_drivers d CROSS JOIN ts_metrics m CROSS JOIN k ORDER BY d.id,m.id,k.i DESC`,
		buckets, newest, HistoryResolutionMS, HistoryResolutionMS, newest, HistoryResolutionMS, newest, HistoryResolutionMS); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var stateBytes, historyBytes int64
	_ = s.db.QueryRow(`SELECT page_count*page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&stateBytes)
	_ = s.history.QueryRow(`SELECT page_count*page_size FROM pragma_page_count(), pragma_page_size()`).Scan(&historyBytes)
	t.Logf("prepared state.db %d MiB, history.db %d MiB, %d history buckets per series", stateBytes>>20, historyBytes>>20, buckets)
}

type latencyRecord struct {
	start   time.Time
	elapsed time.Duration
	err     error
}

type latencyLog struct {
	mu      sync.Mutex
	records []latencyRecord
}

func (l *latencyLog) add(start time.Time, err error) {
	l.mu.Lock()
	l.records = append(l.records, latencyRecord{start, time.Since(start), err})
	l.mu.Unlock()
}

func (l *latencyLog) window(from, to time.Time) []latencyRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []latencyRecord
	for _, r := range l.records {
		if !r.start.Before(from) && r.start.Before(to) {
			out = append(out, r)
		}
	}
	return out
}

func latencySummary(records []latencyRecord) (string, time.Duration, int) {
	if len(records) == 0 {
		return "n=0", 0, 0
	}
	elapsed := make([]time.Duration, len(records))
	failed, over1, over5 := 0, 0, 0
	for i, r := range records {
		elapsed[i] = r.elapsed
		if r.err != nil {
			failed++
		}
		if r.elapsed > time.Second {
			over1++
		}
		if r.elapsed > 5*time.Second {
			over5++
		}
	}
	sort.Slice(elapsed, func(i, j int) bool { return elapsed[i] < elapsed[j] })
	at := func(q float64) time.Duration { return elapsed[min(len(elapsed)-1, int(q*float64(len(elapsed))))] }
	worst := elapsed[len(elapsed)-1]
	return fmt.Sprintf("n=%d p50=%v p95=%v max=%v over_1s=%d over_5s=%d failed=%d",
		len(records), at(0.5).Round(time.Millisecond), at(0.95).Round(time.Millisecond), worst.Round(time.Millisecond), over1, over5, failed), worst, failed
}

func measureSnapshotLatency(t *testing.T, path string) {
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("run FTW_SNAPSHOT_LATENCY_PHASE=prepare first: %v", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tick := 2 * time.Second // Core's default control interval
	if v := os.Getenv("FTW_SNAPSHOT_LATENCY_TICK"); v != "" {
		if tick, err = time.ParseDuration(v); err != nil {
			t.Fatal(err)
		}
	}
	quiet := 20 * time.Second
	if v := os.Getenv("FTW_SNAPSHOT_LATENCY_QUIET"); v != "" {
		if quiet, err = time.ParseDuration(v); err != nil {
			t.Fatal(err)
		}
	}
	var commits, writes latencyLog
	w := s.historyWriter
	// Commit every tick so each control interval yields a sample; the
	// deadline stays at historyCommitTimeout.
	w.commitInterval = tick
	w.commitFn = func(ctx context.Context, batches []historyBatch, ack int64) (historyBatchCommit, error) {
		start := time.Now()
		out, err := s.recordHistoryBatches(ctx, batches, ack)
		commits.add(start, err)
		return out, err
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for i := 1; ; i++ {
			point, samples := latencyTick(time.Now().UnixMilli(), i)
			if err := s.EnqueueTelemetryTick(point, samples, nil); err != nil {
				t.Errorf("tick %d rejected: %v", i, err)
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for i := 1; ; i++ {
			start := time.Now()
			writes.add(start, s.SaveConfig("snapshot-latency-probe", strconv.Itoa(i)))
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()

	begin := time.Now()
	time.Sleep(quiet)
	copyStart := time.Now()
	tmp := path + ".latency.tmp"
	_ = os.Remove(tmp)
	if err := s.SnapshotTo(tmp); err != nil {
		t.Fatalf("snapshot copy: %v", err)
	}
	checkStart := time.Now()
	vdb, err := openRaw(tmp)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := quickCheck(vdb)
	vdb.Close()
	if err != nil || !ok {
		t.Fatalf("snapshot quick_check ok=%v err=%v", ok, err)
	}
	checkEnd := time.Now()
	info, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(tmp)
	time.Sleep(quiet)
	close(stop)
	wg.Wait()
	flushCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.FlushHistory(flushCtx); err != nil {
		t.Fatal(err)
	}
	end := time.Now()

	t.Logf("snapshot %d MiB: copy %v, quick_check %v; tick %v, commit deadline %v",
		info.Size()>>20, checkStart.Sub(copyStart).Round(time.Millisecond), checkEnd.Sub(checkStart).Round(time.Millisecond), tick, w.timeout())
	windows := []struct {
		name     string
		from, to time.Time
		gated    bool
	}{
		{"before", begin, copyStart, false},
		{"copy", copyStart, checkStart, true},
		{"quick_check", checkStart, checkEnd, true},
		{"after", checkEnd, end, false},
	}
	var limit time.Duration
	if v := os.Getenv("FTW_SNAPSHOT_LATENCY_LIMIT"); v != "" {
		if limit, err = time.ParseDuration(v); err != nil {
			t.Fatal(err)
		}
	}
	var report strings.Builder
	for _, win := range windows {
		for _, kind := range []struct {
			name string
			log  *latencyLog
		}{{"history commit", &commits}, {"config write", &writes}} {
			line, worst, failed := latencySummary(kind.log.window(win.from, win.to))
			fmt.Fprintf(&report, "\n%-12s %-15s %s", win.name, kind.name, line)
			if limit > 0 && win.gated && (worst > limit || failed > 0) {
				t.Errorf("%s during snapshot %s: max %v (limit %v), %d failed", kind.name, win.name, worst.Round(time.Millisecond), limit, failed)
			}
		}
	}
	st := s.HistoryWriterStatus()
	t.Logf("latency by window:%s\nwriter: accepted=%d committed=%d rejected=%d commit_failures=%d last_failure=%q",
		report.String(), st.Accepted, st.Committed, st.Rejected, st.CommitFailures, st.LastFailureError)
}
