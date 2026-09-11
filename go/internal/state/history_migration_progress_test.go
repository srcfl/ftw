package state

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMigrationByteRateExcludesPriorProgressAndWaitETA(t *testing.T) {
	m := newHistoryMigration(nil)
	defer m.cancel()
	s := &Store{historyMigration: m}
	unknown, _ := json.Marshal(s.HistoryMigrationStatus())
	if strings.Contains(string(unknown), "source_bytes_total") || strings.Contains(string(unknown), "eta_seconds") {
		t.Fatalf("invented unknown metrics: %s", unknown)
	}
	total, prior := int64(1000), int64(500)
	m.startSourceBytes(&total, &prior)
	m.update(func(st *HistoryMigrationStatus) {
		st.State, st.Phase, st.Activity = "running", "parquet", "importing"
		st.StartedAtMS = time.Now().Add(-20 * time.Second).UnixMilli()
		m.byteStarted = time.Now().Add(-10 * time.Second)
	})
	if st := s.HistoryMigrationStatus(); st.ETASeconds != nil || st.BytesPerSecond != 0 {
		t.Fatalf("old bytes became current throughput: %+v", st)
	}
	m.addSourceBytes(100, true, true)
	st := s.HistoryMigrationStatus()
	if *st.SourceBytesDone != 600 || *st.SourceBytesTotal != 1000 || !st.BytesEstimated || st.BytesPerSecond < 9 || st.BytesPerSecond > 11 || st.ETASeconds == nil || *st.ETASeconds < 40 || *st.ETASeconds > 42 || st.ElapsedMS < 20000 {
		t.Fatalf("wrong byte progress: %+v", st)
	}
	s.historyActivity("waiting_for_live")
	if st := s.HistoryMigrationStatus(); st.ETASeconds != nil {
		t.Fatalf("ETA claimed during blocked writer: %+v", st)
	}
	if prior != 500 {
		t.Fatal("mutated a published status snapshot")
	}
}

func TestCompletedSQLiteImportDoesNotReadSQLite(t *testing.T) {
	s := freshStore(t)
	m := newHistoryMigration(nil)
	s.historyMigration = m
	defer func() { s.historyMigration = nil; m.cancel() }()
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.importSQLiteSamples(context.Background()); err != nil {
		t.Fatalf("completed import touched closed SQLite: %v", err)
	}
}

func TestParquetByteProgressCountsCompressedSourcesAndPartialRows(t *testing.T) {
	path, cold := legacyMigrationFixture(t, 0)
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	first, second := filepath.Join(day, "01.parquet"), filepath.Join(day, "02.parquet")
	if err := writeParquetDay(first, []parquetSampleRow{{TsMs: 1, Driver: "meter", Metric: "power", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(context.Background(), cold); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	points := make([]parquetSampleRow, historyImportRows+13)
	for i := range points {
		points[i] = parquetSampleRow{TsMs: int64(10000 + i), Driver: "meter", Metric: "power", Value: float64(i)}
	}
	if err := writeParquetDay(second, points); err != nil {
		t.Fatal(err)
	}
	f1, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once, unblock sync.Once
	s, err = OpenWithBackgroundHistory(path, cold, func(st HistoryMigrationStatus) {
		if st.BytesEstimated && st.CurrentSourceRowsDone == historyImportRows {
			once.Do(func() { close(reached); <-release })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unblock.Do(func() { close(release) }); s.Close() }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("no partial byte progress")
	}
	st := s.HistoryMigrationStatus()
	want := f1.Size() + estimatedSourceBytes(f2.Size(), historyImportRows, int64(len(points)))
	if st.SourceBytesDone == nil || *st.SourceBytesDone != want || st.SourceBytesTotal == nil || *st.SourceBytesTotal != f1.Size()+f2.Size() || st.Activity != "importing" {
		t.Fatalf("wrong source bytes: %+v", st)
	}
	unblock.Do(func() { close(release) })
	select {
	case <-s.historyMigration.done:
	case <-time.After(10 * time.Second):
		t.Fatal("import did not finish")
	}
	st = s.HistoryMigrationStatus()
	if !st.HistoryComplete || st.BytesEstimated || *st.SourceBytesDone != *st.SourceBytesTotal {
		t.Fatalf("wrong final progress: %+v", st)
	}
}
