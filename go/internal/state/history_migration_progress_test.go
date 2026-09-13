package state

import (
	"context"
	"encoding/json"
	"strings"
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
