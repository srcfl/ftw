package state

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAggregateHistoryPreservesCountsEnvelopeAndLast(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).UnixMilli()
	for i, v := range []float64{100, 100, 9100, 100} {
		p := HistoryPoint{TsMs: base + int64(i)*1000, GridW: v, JSON: `{"status":"charging"}`}
		if err := s.EnqueueTelemetryTick(&p, []Sample{{Driver: "ev", Metric: "power", TsMs: p.TsMs, Value: v}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushHistory(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var raw, n int64
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&raw); err != nil || raw != 0 {
		t.Fatal("raw poll rows retained", raw, err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets`).Scan(&n); err != nil || n != 2 {
		t.Fatal("expected one 10s and one minute summary", n, err)
	}
	rows, err := s.LoadSeriesBucketsOrRaw("ev", "power", base, base+60000, 0)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	p := rows[0]
	if p.N != 4 || p.Min != 100 || p.Max != 9100 || p.V != 2350 || p.Last == nil || *p.Last != 100 || p.ResolutionMS != 10000 || p.FirstMS != base || p.TsMs != base+3000 {
		t.Fatalf("lost evidence: %+v", p)
	}

}

func TestAggregateArchiveRoundTripAndCompaction(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	s.coldDir = filepath.Join(t.TempDir(), "cold")
	day := time.Now().UTC().Truncate(24 * time.Hour)
	base := day.UnixMilli()
	var wantN int64
	var wantSum float64
	for _, offset := range []int64{1000, 2000, 29000, 61000, 62000, 181000} {
		v := float64(offset % 7300)
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "grid_w", TsMs: base + offset, Value: v}}, nil); err != nil {
			t.Fatal(err)
		}
		wantN++
		wantSum += v
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainAggregateHistory(context.Background(), s.coldDir, day.Add(3*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check := func(resolution int64, wantPoints int) {
		t.Helper()
		points, err := s.LoadSeriesBucketsOrRaw("meter", "grid_w", base, base+24*time.Hour.Milliseconds()-1, 0)
		if err != nil || len(points) != wantPoints {
			t.Fatalf("points=%+v err=%v", points, err)
		}
		var n int64
		var sum float64
		for _, p := range points {
			n += p.N
			sum += p.V * float64(p.N)
			if p.ResolutionMS != resolution {
				t.Fatal("wrong resolution", p)
			}
		}
		if n != wantN || math.Abs(sum-wantSum) > 1e-8 {
			t.Fatal("aggregate changed observations", n, sum, wantN, wantSum)
		}
	}
	check(60000, 3)
	if err := s.MaintainAggregateHistory(context.Background(), s.coldDir, day.Add(40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	check(300000, 1)
	if err := s.MaintainAggregateHistory(context.Background(), s.coldDir, day.Add(40*24*time.Hour)); err != nil {
		t.Fatal("repeat", err)
	}
	check(300000, 1)
	var raw int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets`).Scan(&raw); err != nil || raw != 0 {
		t.Fatal("source not pruned", raw, err)
	}
}

func TestAggregateRetryRestartDuplicatesAndOutOfOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Truncate(time.Minute).UnixMilli()
	samples := []Sample{{Driver: "ev", Metric: "power", TsMs: base + 9000, Value: 900}, {Driver: "ev", Metric: "power", TsMs: base + 1000, Value: 100}, {Driver: "ev", Metric: "power", TsMs: base + 1000, Value: 999}}
	seq, err := s.recordHistoryBatch(context.Background(), "uncertain", "same", nil, samples, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	retry, err := s.recordHistoryBatch(context.Background(), "uncertain", "same", nil, samples, nil, 0)
	if err != nil || retry != seq {
		t.Fatal(retry, seq, err)
	}
	if _, err := s.recordHistoryBatch(context.Background(), "second", "different", nil, append(samples, Sample{Driver: "ev", Metric: "power", TsMs: base + 5000, Value: 500}), nil, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadSeriesBucketsOrRaw("ev", "power", base, base+60000, 0)
	if err != nil || len(rows) != 1 || rows[0].N != 3 || rows[0].V != 500 || rows[0].Min != 100 || rows[0].Max != 900 {
		t.Fatal(rows, err)
	}
	latest, err := s.LatestSample("ev", "power")
	if err != nil || latest.Value != 900 || latest.TsMs != base+9000 {
		t.Fatal(latest, err)
	}
	var n int64
	if err := s.history.QueryRow(`SELECT SUM(n) FROM ts_series_hour`).Scan(&n); err != nil || n != 3 {
		t.Fatal(n, err)
	}
}

func TestAggregateCutoffWaitsForAcceptedTick(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute)
	s.historyWriteMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.historyWriteMu.Unlock()
		}
	}()
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "ev", Metric: "power", TsMs: base.UnixMilli() + 1000, Value: 500}}, nil); err != nil {
		t.Fatal(err)
	}
	if got := s.reserveAggregateCutoff(base.Add(time.Hour).UnixMilli()); got != base.UnixMilli() {
		t.Fatal("archived a pending tick", got)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "ev", Metric: "power", TsMs: base.UnixMilli() - 1000, Value: 700}}, nil); err == nil {
		t.Fatal("accepted data behind archive boundary")
	}
	s.historyWriteMu.Unlock()
	locked = false
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := s.reserveAggregateCutoff(base.Add(time.Hour).UnixMilli()); got != base.Add(time.Hour).UnixMilli() {
		t.Fatal(got)
	}
	status := s.HistoryWriterStatus()
	if status.Committed != 1 || status.Rejected != 1 || status.Pending != 0 {
		t.Fatal(status)
	}
}

func TestAggregatePartialDayArchiveAndLateAdmission(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	s.coldDir = t.TempDir()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	for _, hour := range []int{0, 12, 23} {
		ts := day.Add(time.Duration(hour)*time.Hour).UnixMilli() + 1000
		if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: ts, Value: float64(hour + 1)}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainAggregateHistory(context.Background(), s.coldDir, day.Add(36*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadSeriesBucketsOrRaw("meter", "power", day.UnixMilli(), day.Add(24*time.Hour).UnixMilli(), 0)
	if err != nil || len(rows) != 3 || rows[0].ResolutionMS != 60000 || rows[1].ResolutionMS != 10000 {
		t.Fatal(rows, err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: day.UnixMilli() + 2000, Value: 500}}, nil); err == nil {
		t.Fatal("accepted archived timestamp")
	}
	// A later point on the same UTC day is still admissible.
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: day.Add(23*time.Hour).UnixMilli() + 2000, Value: 26}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainAggregateHistory(context.Background(), s.coldDir, day.Add(49*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err = s.LoadSeriesBucketsOrRaw("meter", "power", day.UnixMilli(), day.Add(24*time.Hour).UnixMilli(), 0)
	if err != nil || len(rows) != 3 || rows[2].N != 2 || rows[2].V != 25 {
		t.Fatal(rows, err)
	}
}

func TestAggregatePublicationOverlapAndChangedSource(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	s.coldDir = t.TempDir()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	base := day.UnixMilli()
	write := func(id string, offset int64, v float64) {
		t.Helper()
		_, err := s.recordHistoryBatch(context.Background(), id, id, nil, []Sample{{Driver: "d", Metric: "m", TsMs: base + offset, Value: v}}, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	write("first", 1000, 100)
	dir := filepath.Join(s.coldDir, day.Format("2006/01"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stage, tmp, err := newBucketStage(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer removeBucketStage(stage, tmp)
	b := metricBucket{Driver: "d", Metric: "m", BucketSummary: BucketSummary{StartMS: base, ResolutionMS: 60000, FirstMS: base + 1000, LastMS: base + 1000, N: 1, Sum: 100, Min: 100, Max: 100, Last: 100}}
	if err := stageBuckets(context.Background(), stage, []metricBucket{b}, false); err != nil {
		t.Fatal(err)
	}
	if err := publishBucketStage(context.Background(), stage, filepath.Join(dir, day.Format("02.buckets.parquet"))); err != nil {
		t.Fatal(err)
	}
	write("second", 2000, 300)
	if err := s.pruneBucketMinutes(context.Background(), []metricBucket{b}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.LoadSeriesBucketsOrRaw("d", "m", base, base+60000, 0)
	if err != nil || len(rows) != 1 || rows[0].N != 2 || rows[0].V != 200 || rows[0].ResolutionMS != 10000 {
		t.Fatal("published overlap lost or duplicated source", rows, err)
	}
	if err := s.MaintainAggregateHistory(context.Background(), s.coldDir, day.Add(3*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err = s.LoadSeriesBucketsOrRaw("d", "m", base, base+60000, 0)
	if err != nil || len(rows) != 1 || rows[0].N != 2 || rows[0].ResolutionMS != 60000 {
		t.Fatal(rows, err)
	}
}

func TestAggregateCorruptArchiveRetainsSQLite(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	s.coldDir = t.TempDir()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	base := day.UnixMilli()
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "d", Metric: "m", TsMs: base + 1000, Value: 5}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.coldDir, day.Format("2006/01"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, day.Format("02.buckets.parquet"))
	if err := os.WriteFile(path, []byte("truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainAggregateHistory(context.Background(), s.coldDir, day.Add(3*24*time.Hour)); err == nil {
		t.Fatal("accepted corrupt source")
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_buckets`).Scan(&n); err != nil || n != 2 {
		t.Fatal("lost source", n, err)
	}
}

func TestAggregatePreservesOriginalEnergyObservations(t *testing.T) {
	raw, agg := freshStore(t), freshStore(t)
	if err := agg.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Hour).UnixMilli()
	asset := HardwareEnergyAssetID("maker:car", AssetVehicleCharger)
	for i, off := range []int64{0, 1000, 9000, 11000, 61000, 120000, 3600000, 3601000} {
		at := base + off
		observations := []EnergyObservation{ledgerObservation(asset, AssetVehicleCharger, FlowVehicleCharge, at, energyPtr(float64(i)*100), energyPtr(float64(i%2)*7000))}
		samples := []Sample{{Driver: "ev", Metric: "ev_w", TsMs: at, Value: float64(i%2) * 7000}}
		if err := raw.RecordTickWithOptionalHistory(nil, samples, observations); err != nil {
			t.Fatal(err)
		}
		if err := agg.EnqueueTelemetryTick(nil, samples, observations); err != nil {
			t.Fatal(err)
		}
	}
	if err := agg.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := loadLedgerTestPoints(t, raw, asset, base, base+7200000)
	got := loadLedgerTestPoints(t, agg, asset, base, base+7200000)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("energy changed through gauge aggregation:\ngot %+v\nwant %+v", got, want)
	}
}

func TestAggregateLegacyResumeAndMixedHour(t *testing.T) {
	s := freshStore(t)
	s.coldDir = t.TempDir()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-10 * 24 * time.Hour)
	base := day.UnixMilli()
	dir := filepath.Join(s.coldDir, day.Format("2006/01"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, day.Format("02.parquet"))
	raw := make([]parquetSampleRow, 600)
	for i := range raw {
		raw[i] = parquetSampleRow{Driver: "meter", Metric: "power", TsMs: base + int64(i)*1000, Value: float64(i % 101)}
	}
	if err := writeParquetDay(path, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "meter", Metric: "power", TsMs: base + 700000, Value: 700}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainAggregateHistory(context.Background(), s.coldDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Simulate the durable checkpoint left by an interrupted conversion.
	stagePath := filepath.Join(dir, ".ftw-buckets-"+day.Format("02")+".pending.db")
	stage, err := openBackupDestination(stagePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{bucketStageSchema, `CREATE TABLE bucket_progress(id INTEGER PRIMARY KEY,source_sha256 TEXT,resolution_ms INTEGER,rows_done INTEGER)`} {
		if _, err := stage.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	hash, err := historyFileHashContext(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Exec(`INSERT INTO bucket_progress VALUES(1,?,?,0)`, hash, ArchiveResolutionMS); err != nil {
		t.Fatal(err)
	}
	var prefix []metricBucket
	for _, v := range raw[:300] {
		b := rawBucket(v.TsMs, v.Value)
		b.StartMS = bucketStart(v.TsMs, ArchiveResolutionMS)
		b.ResolutionMS = ArchiveResolutionMS
		prefix = append(prefix, metricBucket{Driver: v.Driver, Metric: v.Metric, BucketSummary: b})
	}
	if err := stageBucketsProgress(context.Background(), stage, prefix, true, 300); err != nil {
		t.Fatal(err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.CompactLegacyHistory(context.Background(), s.coldDir, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("raw source not retired", err)
	}
	points, err := s.LoadSeriesBucketsOrRaw("meter", "power", base, base+3600000, 0)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	var sum float64
	for _, p := range points {
		n += p.N
		sum += p.V * float64(p.N)
	}
	want := 700.0
	for _, p := range raw {
		want += p.Value
	}
	if n != 601 || math.Abs(sum-want) > 1e-7 {
		t.Fatal("resume duplicated or lost measurements", n, sum, want)
	}
	var hourlyN int64
	var hourlySum float64
	if err := s.history.QueryRow(`SELECT SUM(n),SUM(sum_value) FROM ts_series_hour`).Scan(&hourlyN, &hourlySum); err != nil || hourlyN != n || math.Abs(hourlySum-want) > 1e-7 {
		t.Fatal("legacy hourly rebuild lost new aggregates", hourlyN, hourlySum, err)
	}
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: base + 2000, Value: 42}}); !errors.Is(err, ErrCompactedHistory) {
		t.Fatal("accepted individual correction after compaction", err)
	}
}

func TestAggregateRestartAfterPublishedArchiveBeforePrune(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.db")
	cold := filepath.Join(root, "cold")
	s, err := OpenWithLegacyHistory(path, cold)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	base := day.UnixMilli()
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "d", Metric: "m", TsMs: base + 1000, Value: 10}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`CREATE TRIGGER fail_bucket_prune BEFORE DELETE ON ts_buckets BEGIN SELECT RAISE(ABORT,'injected prune failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainAggregateHistory(context.Background(), cold, day.Add(48*time.Hour)); err == nil {
		t.Fatal("ignored prune failure")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithLegacyHistory(path, cold)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "d", Metric: "m", TsMs: base + 2000, Value: 20}}, nil); err == nil {
		t.Fatal("forgot closed interval after restart")
	}
	points, err := s.LoadSeriesBucketsOrRaw("d", "m", base, base+60000, 0)
	if err != nil || len(points) != 1 || points[0].N != 1 || points[0].ResolutionMS != 10000 {
		t.Fatal(points, err)
	}
	if _, err := s.history.Exec(`DROP TRIGGER fail_bucket_prune`); err != nil {
		t.Fatal(err)
	}
	if err := s.MaintainAggregateHistory(context.Background(), cold, day.Add(48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	points, err = s.LoadSeriesBucketsOrRaw("d", "m", base, base+60000, 0)
	if err != nil || len(points) != 1 || points[0].N != 1 || points[0].ResolutionMS != 60000 {
		t.Fatal(points, err)
	}
}
