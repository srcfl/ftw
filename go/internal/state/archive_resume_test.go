package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSampleArchiveResumesCommittedCopyAndPreservesValues(t *testing.T) {
	s := freshStore(t)
	cold := t.TempDir()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-72 * time.Hour)
	const count = 30000
	samples := make([]Sample, count)
	for i := range samples {
		samples[i] = Sample{Driver: fmt.Sprint("meter", i%2), Metric: "power", TsMs: day.UnixMilli() + int64(i), Value: float64(i) - 15000}
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := s.archiveSampleDay(ctx, cold, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli())
		done <- err
	}()
	deadline := time.After(10 * time.Second)
	for s.HistoryMaintenanceStatus().RowsDone < 1024 {
		select {
		case err := <-done:
			cancel()
			t.Fatalf("archive ended before interruption: %v", err)
		case <-deadline:
			cancel()
			<-done
			t.Fatal("no copy progress")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	path := filepath.Join(cold, day.Format("2006/01/.ftw-archive-02.samples.db"))
	stage, job, err := openArchiveJob(context.Background(), path, sampleStageSchema)
	if err != nil {
		t.Fatal(err)
	}
	stage.Close()
	if job.CopiedRows < 1024 || job.Phase != "copy" {
		t.Fatalf("lost committed cursor: %+v", job)
	}
	// A new Store instance has none of the old job's in-memory state.
	restarted := &Store{history: s.history, historyWriter: s.historyWriter}
	_, file, err := restarted.archiveSampleDay(context.Background(), cold, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := walkParquetRows(context.Background(), file, func(batch []parquetSampleRow) error {
		for _, r := range batch {
			i := int(r.TsMs - day.UnixMilli())
			if i < 0 || i >= count || r.Driver != samples[i].Driver || r.Metric != "power" || r.Value != samples[i].Value {
				return fmt.Errorf("changed row %d", i)
			}
			rows++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if rows != count || left != 0 {
		t.Fatalf("archive rows=%d source rows=%d", rows, left)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("finished scratch retained: %v", err)
	}
}

func TestResumedArchiveRefusesChangedPublishedFile(t *testing.T) {
	s := freshStore(t)
	cold := t.TempDir()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-72 * time.Hour)
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: day.UnixMilli() + 1, Value: 42}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`CREATE TRIGGER fail_prune BEFORE DELETE ON ts_samples BEGIN SELECT RAISE(ABORT,'stop prune'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.archiveSampleDay(context.Background(), cold, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli()); err == nil {
		t.Fatal("expected prune failure")
	}
	file := filepath.Join(cold, day.Format("2006/01/02.parquet"))
	if err := os.WriteFile(file, []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`DROP TRIGGER fail_prune`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.archiveSampleDay(context.Background(), cold, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli()); err == nil {
		t.Fatal("deleted source without the verified archive")
	}
	var count int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("source=%d err=%v", count, err)
	}
}

func TestBackupPausesMaintenanceWithoutStoppingWriter(t *testing.T) {
	s := freshStore(t)
	if err := s.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.MaintainHistory(context.Background(), t.TempDir(), 0, time.Now()) }()
	deadline := time.After(5 * time.Second)
	for s.HistoryMaintenanceStatus().Phase != "aggregate_archive" {
		select {
		case <-deadline:
			t.Fatal("maintenance did not start")
		case <-time.After(time.Millisecond):
		}
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
	if err := s.MaintainHistory(ctx, t.TempDir(), 0, time.Now()); err != nil {
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

func TestArchiveTurnsFinishWithoutRepeatingPublishedFiles(t *testing.T) {
	for _, aggregate := range []bool{false, true} {
		t.Run(fmt.Sprint("aggregate=", aggregate), func(t *testing.T) {
			s := freshStore(t)
			cold := t.TempDir()
			day := time.Now().UTC().Truncate(24 * time.Hour).Add(-72 * time.Hour)
			if aggregate {
				if err := s.EnableHistoryAggregation(); err != nil {
					t.Fatal(err)
				}
			}
			samples := make([]Sample, 3000)
			for i := range samples {
				samples[i] = Sample{Driver: fmt.Sprint("meter", i%3), Metric: "power", TsMs: day.UnixMilli() + int64(i/3)*60000, Value: float64(i)}
			}
			if aggregate {
				// Seed normal-sized ticks; a single 3,000-metric tick exceeds
				// the writer transaction budget under the race detector.
				for start := 0; start < len(samples); start += 64 {
					if err := s.EnqueueTelemetryTick(nil, samples[start:min(start+64, len(samples))], nil); err != nil {
						t.Fatal(err)
					}
					if err := s.FlushHistory(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
			} else if err := s.RecordSamples(samples); err != nil {
				t.Fatal(err)
			}
			finished := false
			for turn := 0; turn < 30; turn++ {
				ctx := context.WithValue(context.Background(), archiveTurnKey{}, time.Now().Add(-time.Second))
				restarted := &Store{history: s.history, historyWriter: s.historyWriter}
				var err error
				if aggregate {
					err = restarted.archiveBucketDay(ctx, cold, day, day.Add(24*time.Hour).UnixMilli())
				} else {
					_, _, err = restarted.archiveSampleDay(ctx, cold, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli())
				}
				if err == nil {
					finished = true
					break
				}
				if !errors.Is(err, errArchiveTurnComplete) {
					t.Fatal(err)
				}
			}
			if !finished {
				t.Fatal("archive kept restarting instead of finishing")
			}
			var count int64
			var sum float64
			if aggregate {
				err := walkBucketFile(context.Background(), filepath.Join(cold, day.Format("2006/01/02.buckets.parquet")), func(b metricBucket) error { count += b.N; sum += b.Sum; return nil })
				if err != nil {
					t.Fatal(err)
				}
			} else {
				err := walkParquetRows(context.Background(), filepath.Join(cold, day.Format("2006/01/02.parquet")), func(rows []parquetSampleRow) error {
					for _, r := range rows {
						count++
						sum += r.Value
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if count != 3000 || sum != 4498500 {
				t.Fatalf("changed observations count=%d sum=%f", count, sum)
			}
		})
	}
}

func TestArchiveResumePreservesReplacementSource(t *testing.T) {
	s := freshStore(t)
	cold := t.TempDir()
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-72 * time.Hour)
	path := filepath.Join(cold, day.Format("2006/01/02.parquet"))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := parquetSampleRow{TsMs: day.UnixMilli(), Driver: "meter", Metric: "power", Value: 10}
	if err := writeParquetDay(path, []parquetSampleRow{original}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSamples([]Sample{{Driver: "meter", Metric: "power", TsMs: day.UnixMilli() + 1000, Value: 20}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), archiveTurnKey{}, time.Now().Add(-time.Second))
	if _, _, err := s.archiveSampleDay(ctx, cold, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli()); !errors.Is(err, errArchiveTurnComplete) {
		t.Fatal(err)
	}
	original.Value = 99
	if err := writeParquetDay(path, []parquetSampleRow{original}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.archiveSampleDay(context.Background(), cold, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := walkParquetRows(context.Background(), path, func(rows []parquetSampleRow) error {
		for _, r := range rows {
			count++
			if r.TsMs == original.TsMs && r.Value != 99 {
				return errors.New("resumed stale scratch overwrote replacement source")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("lost source rows: %d", count)
	}
}
