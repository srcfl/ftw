package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestPartialHourQueriesAndLiveWritesDuringArchiveBuild(t *testing.T) {
	s := freshStore(t)
	base := time.Now().UTC().Truncate(time.Hour).Add(-7 * 24 * time.Hour).UnixMilli()
	if err := s.RecordSamples([]Sample{
		{Driver: "meter", Metric: "power", TsMs: base, Value: 999},
		{Driver: "meter", Metric: "power", TsMs: base + 1, Value: 10},
		{Driver: "meter", Metric: "power", TsMs: base + 24*seriesHourMs, Value: 20},
		{Driver: "meter", Metric: "power", TsMs: base + 72*seriesHourMs + 1, Value: 30},
		{Driver: "meter", Metric: "power", TsMs: base + 72*seriesHourMs + 2, Value: 999},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureSeriesHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The slow staging/compression phase owns the archive operation lock.
	// It must not exclude queries, including exact partial-hour boundaries.
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	for i := 0; i < 8; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		points, err := s.LoadSeriesBucketsContext(ctx, "meter", "power", base+1, base+72*seriesHourMs+1, 400)
		cancel()
		if err != nil {
			t.Fatal("archive build blocked a completed hourly query:", err)
		}
		var n int64
		var sum float64
		for _, p := range points {
			n += p.N
			sum += p.V * float64(p.N)
		}
		if n != 3 || sum != 60 {
			t.Fatalf("partial boundaries included or lost samples: n=%d sum=%v", n, sum)
		}
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: base + 96*seriesHourMs + int64(i), Value: float64(i)}}, nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		err = s.FlushHistory(ctx)
		cancel()
		if err != nil {
			t.Fatal("archive build blocked live commit:", err)
		}
	}
	if st := s.HistoryWriterStatus(); st.Accepted != 8 || st.Committed != 8 || st.Pending != 0 || st.Rejected != 0 || st.LastError != "" {
		t.Fatalf("live writes did not remain durable: %+v", st)
	}
}

func TestDenseArchiveHourReadsOutsideWriteBudgetAndRetriesSnapshot(t *testing.T) {
	reading, release := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	defer released.Do(func() { close(release) })
	fn := fmt.Sprintf("test_archive_slow_hour_%d", time.Now().UnixNano())
	if err := sqlite.RegisterScalarFunction(fn, 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		entered.Do(func() { close(reading); <-release })
		return args[0], nil
	}); err != nil {
		t.Fatal(err)
	}
	s := freshStore(t)
	base := time.Now().UTC().Truncate(time.Hour).UnixMilli()
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: base + 1, Value: 10}}); err != nil {
		t.Fatal(err)
	}
	d, err := s.driverID("meter")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.metricID("power", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`ALTER TABLE ts_samples RENAME TO slow_archive_source`,
		`CREATE VIEW ts_samples AS SELECT driver_id,metric_id,ts_ms,` + fn + `(value) AS value FROM slow_archive_source`,
	} {
		if _, err := s.history.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.mergeArchivedHour(ctx, d, m, base, map[int64]float64{base + 2: 20}) }()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal("hour read did not start")
	}
	// A dense/slow scan may exceed the write budget. It must not own the
	// live mutex, and its snapshot must be retried if raw data changes.
	lockCtx, stopLock := context.WithTimeout(ctx, 200*time.Millisecond)
	err = lockContext(lockCtx, s.historyWriteMu.TryLock)
	stopLock()
	if err != nil {
		t.Fatal("slow hour read held the live writer mutex:", err)
	}
	s.historyWriteMu.Unlock()
	if _, err := s.history.ExecContext(ctx, `INSERT INTO slow_archive_source(driver_id,metric_id,ts_ms,value) VALUES(?,?,?,?)`, d, m, base+3, 30); err != nil {
		t.Fatal(err)
	}
	time.Sleep(archiveWriteTimeout + 100*time.Millisecond)
	released.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal("dense hour could not resume:", err)
	}
	var n int64
	var sum float64
	if err := s.history.QueryRow(`SELECT n,sum_value FROM ts_series_hour WHERE driver_id=? AND metric_id=? AND hour_ms=?`, d, m, base).Scan(&n, &sum); err != nil || n != 3 || sum != 60 {
		t.Fatalf("snapshot retry lost the late raw sample: n=%d sum=%v err=%v", n, sum, err)
	}
}

func TestPreparedArchiveHourRetriesInnerWriteDeadline(t *testing.T) {
	s := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	attempts := 0
	err := s.archiveTransaction(ctx, func(context.Context, *sql.Tx) error {
		attempts++
		return nil
	}, func(ctx context.Context, tx *sql.Tx) error {
		if attempts == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO history_migrations(name) VALUES ('prepared-hour-retried')`)
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("inner deadline abandoned the prepared hour: attempts=%d err=%v", attempts, err)
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_migrations WHERE name='prepared-hour-retried'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("retried write missing: %d %v", n, err)
	}
}

func TestArchivePruneKeepsReaderViewAndLateCorrections(t *testing.T) {
	s := freshStore(t)
	s.coldDir = t.TempDir()
	base := time.Now().UTC().Add(-20 * 24 * time.Hour).Truncate(24 * time.Hour)
	path := filepath.Join(s.coldDir, base.Format("2006/01/02.parquet"))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var archive []parquetSampleRow
	var copied, current []Sample
	for i := 1; i <= 3; i++ {
		ts := base.UnixMilli() + int64(i)*1000
		archive = append(archive, parquetSampleRow{TsMs: ts, Driver: "meter", Metric: "power", Value: float64(i * 10)})
		copied = append(copied, Sample{TsMs: ts, Driver: "meter", Metric: "power", Value: float64(i * 10)})
		current = append(current, copied[i-1])
	}
	current[1].Value = 99 // A correction arrived after the archive copy.
	if err := s.RecordSamples(current); err != nil {
		t.Fatal(err)
	}
	if err := writeParquetDay(path, archive); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.resolveSamples(copied)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reading, release := make(chan struct{}), make(chan struct{})
	var entered, released sync.Once
	defer released.Do(func() { close(release) })
	readDone := make(chan error, 1)
	seen := map[int64]float64{}
	go func() {
		readDone <- s.walkMergedSeries(ctx, s.coldDir, "meter", "power", base.UnixMilli(), base.Add(time.Hour).UnixMilli(), func(ts int64, v float64) error {
			entered.Do(func() { close(reading); <-release })
			seen[ts] = v
			return nil
		})
	}()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal("reader did not start")
	}
	type pruneResult struct {
		n   int64
		err error
	}
	pruned := make(chan pruneResult, 1)
	go func() { n, err := s.pruneArchivedSamples(ctx, resolved); pruned <- pruneResult{n, err} }()
	select {
	case r := <-pruned:
		t.Fatalf("pruning crossed an active reader: %+v", r)
	case <-time.After(30 * time.Millisecond):
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: time.Now().UnixMilli(), Value: 7000}}, nil); err != nil {
		t.Fatal(err)
	}
	live, done := context.WithTimeout(ctx, 500*time.Millisecond)
	err = s.FlushHistory(live)
	done()
	if err != nil {
		t.Fatal("pruning waited for a reader while holding the live writer:", err)
	}
	released.Do(func() { close(release) })
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	r := <-pruned
	if r.err != nil || r.n != 2 {
		t.Fatalf("prune must preserve the late correction: %+v", r)
	}
	points, err := s.LoadSeries("meter", "power", base.UnixMilli(), base.Add(time.Hour).UnixMilli(), 0)
	if err != nil || len(points) != 3 || len(seen) != 3 {
		t.Fatalf("handover lost or duplicated samples: before=%v after=%v err=%v", seen, points, err)
	}
	for _, p := range points {
		if seen[p.TsMs] != p.Value {
			t.Fatalf("handover changed %d: %v -> %v", p.TsMs, seen[p.TsMs], p.Value)
		}
	}
	if points[1].Value != 99 {
		t.Fatal("archive replaced a later raw value")
	}
}

func TestArchivePublicationAndRemovalRespectReaderCancellation(t *testing.T) {
	s := freshStore(t)
	path, tmp := filepath.Join(t.TempDir(), "day.parquet"), filepath.Join(t.TempDir(), "verified.tmp")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("verified replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	s.archiveViewMu.RLock()
	for _, operation := range []func(context.Context) error{
		func(ctx context.Context) error { return s.replaceArchive(ctx, tmp, path) },
		func(ctx context.Context) error { return s.removeArchive(ctx, path) },
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		err := operation(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			s.archiveViewMu.RUnlock()
			t.Fatalf("blocked operation: %v", err)
		}
	}
	s.archiveViewMu.RUnlock()
	if b, err := os.ReadFile(path); err != nil || string(b) != "original" {
		t.Fatalf("canceled operation changed source: %q %v", b, err)
	}
	if err := s.replaceArchive(context.Background(), tmp, path); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "verified replacement" {
		t.Fatalf("replacement missing: %q %v", b, err)
	}
}

func TestArchiveSQLiteBusyDoesNotConsumeLiveCommitBudget(t *testing.T) {
	s := freshStore(t)
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: 1, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	s.historyWriter.commitInterval = 0
	s.historyWriter.commitTimeout = time.Second
	var expired atomic.Int32
	s.historyWriter.commitFn = func(ctx context.Context, batches []historyBatch, ack int64) (historyBatchCommit, error) {
		out, err := s.recordHistoryBatches(ctx, batches, ack)
		if errors.Is(err, context.DeadlineExceeded) {
			expired.Add(1)
		}
		return out, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	blocker, err := s.history.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	if _, err := blocker.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer blocker.ExecContext(context.Background(), `ROLLBACK`)
	archiveCtx, stopArchive := context.WithTimeout(ctx, 150*time.Millisecond)
	defer stopArchive()
	attempted := make(chan struct{})
	var once sync.Once
	archived := make(chan error, 1)
	go func() {
		archived <- s.writeArchiveBatch(archiveCtx, func(ctx context.Context, tx *sql.Tx) error {
			once.Do(func() { close(attempted) })
			_, err := tx.ExecContext(ctx, `DELETE FROM ts_samples WHERE ts_ms=1`)
			return err
		})
	}()
	select {
	case <-attempted:
	case <-ctx.Done():
		t.Fatal("archive did not attempt the write")
	}
	started := time.Now()
	for i := 0; i < 24; i++ {
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: int64(i + 2), Value: float64(i)}}, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// The archive must finish cancellation while the external SQL lock is
	// still held, not wait for SQLite's default five-second busy timeout.
	select {
	case err := <-archived:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("archive cancellation: %v", err)
		}
	case <-time.After(350 * time.Millisecond):
		t.Fatal("archive kept the writer mutex inside SQLite busy handling")
	}
	if _, err := blocker.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.HistoryWriterStatus(); st.Committed != 24 || st.Pending != 0 || st.Rejected != 0 || st.LastError != "" {
		t.Fatalf("temporary archive/database lock exhausted live writes: %+v", st)
	}
	if time.Since(started) > time.Second {
		t.Fatal("live commit consumed its full budget")
	}
	if expired.Load() != 0 {
		t.Fatal("database contention exhausted a live transaction budget")
	}
	// Reserve the entire pool to verify no zero-busy connection leaked.
	if err := blocker.Close(); err != nil {
		t.Fatal(err)
	}
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		c, err := s.history.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var busy int
		if err := c.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil || busy != 5000 {
			t.Fatalf("connection busy timeout leaked: %d %v", busy, err)
		}
	}
}

func TestSlowArchiveTransactionYieldsBeforeLiveDeadline(t *testing.T) {
	s := freshStore(t)
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: 1, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	s.historyWriter.commitInterval = 0
	s.historyWriter.commitTimeout = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	writing := make(chan struct{})
	archived := make(chan error, 1)
	go func() {
		archived <- s.writeArchiveBatch(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM ts_samples WHERE ts_ms=1`); err != nil {
				return err
			}
			close(writing)
			<-ctx.Done() // Synthetic slow IO while the archive owns SQLite's writer.
			return ctx.Err()
		})
	}()
	select {
	case <-writing:
	case <-ctx.Done():
		t.Fatal("archive did not start")
	}
	for i := 0; i < 40; i++ {
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: int64(i + 2), Value: float64(i)}}, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := <-archived; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow archive was not bounded: %v", err)
	}
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.HistoryWriterStatus(); st.Accepted != 40 || st.Committed != 40 || st.Pending != 0 || st.Rejected != 0 || st.LastError != "" {
		t.Fatalf("slow archive exhausted the live commit budget: %+v", st)
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&n); err != nil || n != 41 {
		t.Fatalf("rollback or live samples lost: %d %v", n, err)
	}
}

func TestSlowParquetStagingDoesNotBlockPartialHours(t *testing.T) {
	s := freshStore(t)
	s.coldDir = t.TempDir()
	base := time.Now().UTC().Truncate(24 * time.Hour).Add(-7 * 24 * time.Hour)
	first, last := base.UnixMilli()+1, base.Add(72*time.Hour).UnixMilli()+1
	samples := []Sample{{Driver: "meter", Metric: "power", TsMs: first, Value: 10}, {Driver: "meter", Metric: "power", TsMs: last, Value: 20}}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureSeriesHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.coldDir, base.Format("2006/01/02.parquet"))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	stage, err := openArchiveStage(filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if err := insertArchiveRows(context.Background(), stage, []parquetSampleRow{{Driver: "meter", Metric: "power", TsMs: first, Value: 10}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Reserving the stage's sole connection pauses the real publication path
	// before compression/readback, without slowing the primary history DB.
	blocked, err := stage.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	published := make(chan error, 1)
	go func() { published <- s.publishStagedSamples(ctx, path, stage) }()
	for stage.Stats().WaitCount == 0 {
		select {
		case err := <-published:
			t.Fatalf("publisher did not wait for staging: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	readCtx, stopRead := context.WithTimeout(ctx, 500*time.Millisecond)
	points, err := s.LoadSeriesBucketsContext(readCtx, "meter", "power", first, last, 400)
	stopRead()
	if err != nil || len(points) != 2 {
		t.Fatalf("staging blocked exact boundary query: %v %v", points, err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: time.Now().UnixMilli(), Value: 7000}}, nil); err != nil {
		t.Fatal(err)
	}
	live, stopLive := context.WithTimeout(ctx, 500*time.Millisecond)
	err = s.FlushHistory(live)
	stopLive()
	if err != nil {
		t.Fatal("staging blocked live commit:", err)
	}
	if err := blocked.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if err := VerifyParquetFile(ctx, path); err != nil {
		t.Fatal(err)
	}
	points, err = s.LoadSeriesBucketsContext(ctx, "meter", "power", first, last, 400)
	var n int64
	for _, p := range points {
		n += p.N
	}
	if err != nil || n != 2 {
		t.Fatalf("published overlap duplicated or lost data: %v %v", points, err)
	}
}

func TestHistoryBatchLockWaitHonorsAttemptDeadline(t *testing.T) {
	s := freshStore(t)
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: 1, Value: 1}}); err != nil {
		t.Fatal(err)
	}
	s.historyWriteMu.Lock()
	var released sync.Once
	defer released.Do(s.historyWriteMu.Unlock)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.recordHistoryBatches(ctx, []historyBatch{{id: "canceled-before-write", hash: "same-payload", payload: historyPayload{Samples: []Sample{{Driver: "meter", Metric: "power", TsMs: 2, Value: 2}}}}}, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lock cancellation: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("writer ignored its deadline while waiting for the mutex")
	}
	released.Do(s.historyWriteMu.Unlock)
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples WHERE ts_ms=2`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("canceled attempt wrote data: %d %v", n, err)
	}
}
