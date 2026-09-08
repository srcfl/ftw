package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A frozen SQLite source beside a fresh DuckDB destination, as on first update.
func legacyMigrationFixture(t *testing.T, n int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	cold := filepath.Join(dir, "cold")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO ts_drivers(id,name) VALUES(7,'meter'); INSERT INTO ts_metrics(id,name,unit) VALUES(9,'power','W'); DELETE FROM config WHERE key LIKE 'history_%'`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO ts_samples(driver_id,metric_id,ts_ms,value) VALUES (7,9,?,?)`)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, err := stmt.Exec(i, float64(i)/7); err != nil {
			t.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO energy_ledger_cursors VALUES ('site','import','counter',1234,10)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(historyDatabasePath(path)); err != nil {
		t.Fatal(err)
	}
	return path, cold
}

func TestBackgroundHistoryKeepsLiveWritesAcrossInterruptedImport(t *testing.T) {
	path, cold := legacyMigrationFixture(t, historyImportRows*3+11)
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	points := make([]parquetSampleRow, historyImportRows*2+17)
	for i := range points {
		points[i] = parquetSampleRow{TsMs: int64(5000 + i), Driver: "meter", Metric: "power", Value: float64(i) + 0.25}
	}
	if err := writeParquetDay(filepath.Join(day, "01.parquet"), points); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s, err := OpenWithBackgroundHistory(path, cold, func(st HistoryMigrationStatus) {
		if st.Phase == "sqlite" && st.RowsDone == historyImportRows {
			once.Do(func() { close(reached); <-release })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("import never reached first committed chunk")
	}
	primary := s.history
	s.historyWriter.maintenanceRowsLimit = 2
	var cursor float64
	if err := s.history.QueryRow(`SELECT value FROM energy_ledger_cursors WHERE asset_id='site'`).Scan(&cursor); err != nil || cursor != 1234 {
		close(release)
		t.Fatalf("accounting was not seeded: %v %v", cursor, err)
	}
	if s.HistoryMigrationStatus().HistoryComplete {
		close(release)
		t.Fatal("partial history reported complete")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	live := []Sample{{TsMs: 3000, Driver: "meter", Metric: "power", Value: 900}, {TsMs: 8000, Driver: "meter", Metric: "power", Value: 901}, {TsMs: 999999, Driver: "new meter", Metric: "new power", Value: 902}}
	if err := s.EnqueueTelemetryTick(nil, live, nil); err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := s.FlushHistory(ctx); err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := s.BackupToCompressed(filepath.Join(filepath.Dir(path), "partial.gz")); err == nil {
		close(release)
		t.Fatal("published an incomplete history backup")
	}
	// Cancel after a durable chunk, then close the real database. The next open
	// must keep live data and resume from the committed source cursor.
	s.historyMigration.cancel()
	close(release)
	<-s.historyMigration.done
	if s.history != primary {
		t.Fatal("import replaced the live database")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithBackgroundHistory(path, cold, nil)
	if err != nil {
		t.Fatal(err)
	}
	primary = s.history
	var reads int
	for {
		select {
		case <-s.historyMigration.done:
			goto finished
		default:
		}
		if _, err := s.LoadSeries("meter", "power", 0, 10000, 48); err != nil {
			t.Fatal(err)
		}
		if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 1000000 + int64(reads), Driver: "live", Metric: "power", Value: float64(reads)}}, nil); err != nil {
			t.Fatal(err)
		}
		if err := s.FlushHistory(ctx); err != nil {
			t.Fatal(err)
		}
		reads++
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
finished:
	if s.history != primary {
		t.Fatal("background import replaced the primary database")
	}
	st := s.HistoryMigrationStatus()
	if !st.HistoryComplete || st.State != "complete" || st.FilesDone != 1 {
		t.Fatalf("migration=%+v", st)
	}
	if reads == 0 {
		t.Fatal("concurrent reader/writer did not run")
	}
	got, err := s.LoadSeries("meter", "power", 0, 10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]float64{}
	for i := 1; i <= historyImportRows*3+11; i++ {
		want[int64(i)] = float64(i) / 7
	}
	for _, p := range points {
		if _, ok := want[p.TsMs]; !ok {
			want[p.TsMs] = p.Value
		}
	}
	want[3000], want[8000] = 900, 901
	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for _, p := range got {
		if expected, ok := want[p.TsMs]; !ok || historyFloatBits(expected) != historyFloatBits(p.Value) {
			t.Fatalf("sample %+v expected %.17g", p, expected)
		}
	}
	if newest, err := s.LatestSample("new meter", "new power"); err != nil || newest.Value != 902 {
		t.Fatalf("live catalog/sample lost: %+v %v", newest, err)
	}
	if _, err := os.Stat(filepath.Join(day, "01.parquet")); err != nil {
		t.Fatal("original Parquet source removed", err)
	}
	if s.HistoryWriterStatus().Rejected != 0 {
		t.Fatalf("writer=%+v", s.HistoryWriterStatus())
	}
}

func TestBackgroundHistoryFailureKeepsCoreStoreUsable(t *testing.T) {
	path, cold := legacyMigrationFixture(t, 3)
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(day, "01.parquet")
	if err := os.WriteFile(file, []byte("invalid parquet"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenWithBackgroundHistory(path, cold, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case <-s.historyMigration.done:
	case <-time.After(10 * time.Second):
		t.Fatal("import did not stop")
	}
	st := s.HistoryMigrationStatus()
	if st.State != "failed" || st.HistoryComplete || st.LastError == "" {
		t.Fatalf("migration=%+v", st)
	}
	if err := s.RecordSamples([]Sample{{Driver: "live", Metric: "power", TsMs: 99, Value: 42}}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestSample("live", "power"); err != nil || got.Value != 42 {
		t.Fatalf("live store=%+v %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	offline, err := OpenBackupSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer offline.Close()
	if err := offline.BackupToCompressed(filepath.Join(filepath.Dir(path), "partial-offline.gz")); err == nil {
		t.Fatal("offline export omitted unfinished history")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(fmt.Errorf("source was removed: %w", err))
	}
}

func TestBackgroundParquetResumesAfterCommittedChunk(t *testing.T) {
	path, cold := legacyMigrationFixture(t, 0)
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(day, "01.parquet")
	points := make([]parquetSampleRow, historyImportRows*2+19)
	for i := range points {
		points[i] = parquetSampleRow{TsMs: int64(i + 1), Driver: "meter", Metric: "power", Value: float64(i)}
	}
	if err := writeParquetDay(file, points); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s, err := OpenWithBackgroundHistory(path, cold, func(st HistoryMigrationStatus) {
		if st.Phase == "parquet" && st.CurrentSourceRowsDone == historyImportRows {
			once.Do(func() { close(reached); <-release })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		close(release)
		s.Close()
		t.Fatal("no committed Parquet chunk")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 3000, Driver: "meter", Metric: "power", Value: 99999}}, nil); err != nil {
		close(release)
		s.Close()
		t.Fatal(err)
	}
	if err := s.FlushHistory(ctx); err != nil {
		close(release)
		s.Close()
		t.Fatal(err)
	}
	s.historyMigration.cancel()
	close(release)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	resumed := int64(-1)
	s, err = OpenWithBackgroundHistory(path, cold, func(st HistoryMigrationStatus) {
		if st.Phase == "parquet" && st.CurrentSourceRowsTotal > 0 && resumed < 0 {
			resumed = st.CurrentSourceRowsDone
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case <-s.historyMigration.done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !s.HistoryMigrationStatus().HistoryComplete || resumed != historyImportRows {
		t.Fatalf("resumed=%d status=%+v", resumed, s.HistoryMigrationStatus())
	}
	got, err := s.LoadSeries("meter", "power", 0, 10000, 0)
	if err != nil || len(got) != len(points) {
		t.Fatalf("rows=%d %v", len(got), err)
	}
	for i, p := range got {
		want := float64(i)
		if p.TsMs == 3000 {
			want = 99999
		}
		if p.Value != want {
			t.Fatalf("sample %+v expected %v", p, want)
		}
	}
}

func TestBackgroundManifestDetectsSourceLostBeforeFirstChunk(t *testing.T) {
	path, cold := legacyMigrationFixture(t, 1)
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(day, "01.parquet")
	if err := writeParquetDay(file, []parquetSampleRow{{TsMs: 99, Driver: "meter", Metric: "power", Value: 42}}); err != nil {
		t.Fatal(err)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s, err := OpenWithBackgroundHistory(path, cold, func(st HistoryMigrationStatus) {
		if st.Phase == "sqlite" {
			once.Do(func() { close(reached); <-release })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	<-reached
	if err := os.Remove(file); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	select {
	case <-s.historyMigration.done:
	case <-time.After(10 * time.Second):
		t.Fatal("missing source did not stop import")
	}
	if st := s.HistoryMigrationStatus(); st.State != "failed" || st.HistoryComplete {
		t.Fatalf("lost source reported complete: %+v", st)
	}
}

func TestHistoricalImportRemovesOnlyOwnedAbandonedStaging(t *testing.T) {
	s := freshStore(t)
	abandoned := s.historyPath + ".import-abandoned"
	ordinary := s.historyPath + ".original-source"
	for _, dir := range []string{abandoned, ordinary} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("keep source"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cold := t.TempDir()
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(day, "01.parquet")
	if err := writeParquetDay(source, []parquetSampleRow{{TsMs: 1, Driver: "meter", Metric: "power", Value: 42}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(context.Background(), cold); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abandoned); !os.IsNotExist(err) {
		t.Fatalf("abandoned staging still exists: %v", err)
	}
	for _, path := range []string{source, filepath.Join(ordinary, "marker"), s.mainDBPath, s.historyPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("removed retained source %s: %v", path, err)
		}
	}
	var receipts int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM history_parquet_sources`).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("receipt=%d %v", receipts, err)
	}
}

func TestHistoricalImportSurvivesAbruptProcessExit(t *testing.T) {
	if phase := os.Getenv("FTW_MIGRATION_CRASH_PHASE"); phase != "" {
		ready := make(chan struct{})
		s, err := OpenWithBackgroundHistory(os.Getenv("FTW_MIGRATION_CRASH_DB"), os.Getenv("FTW_MIGRATION_CRASH_COLD"), func(st HistoryMigrationStatus) {
			if st.State == "running" {
				<-ready
			}
			if st.Phase == phase && st.CurrentSource != "" && st.CurrentSourceRowsDone >= historyImportRows {
				os.Exit(23)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.RecordSamples([]Sample{{TsMs: 999999, Driver: "live before crash", Metric: "power", Value: 123}}); err != nil {
			t.Fatal(err)
		}
		close(ready)
		<-s.historyMigration.done
		t.Fatal("child did not exit after a committed chunk")
	}
	for _, phase := range []string{"sqlite", "parquet"} {
		t.Run(phase, func(t *testing.T) {
			path, cold := legacyMigrationFixture(t, historyImportRows*2+9)
			if phase == "parquet" {
				day := filepath.Join(cold, "2026", "01")
				if err := os.MkdirAll(day, 0700); err != nil {
					t.Fatal(err)
				}
				points := make([]parquetSampleRow, historyImportRows*2+9)
				for i := range points {
					points[i] = parquetSampleRow{TsMs: int64(10000 + i), Driver: "archive", Metric: "power", Value: float64(i)}
				}
				if err := writeParquetDay(filepath.Join(day, "01.parquet"), points); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHistoricalImportSurvivesAbruptProcessExit$")
			child.Env = append(os.Environ(), "FTW_MIGRATION_CRASH_PHASE="+phase, "FTW_MIGRATION_CRASH_DB="+path, "FTW_MIGRATION_CRASH_COLD="+cold)
			out, err := child.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 {
				t.Fatalf("child did not stop at durable progress: %v %s", err, out)
			}
			s, err := OpenWithBackgroundHistory(path, cold, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			select {
			case <-s.historyMigration.done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if !s.HistoryMigrationStatus().HistoryComplete {
				t.Fatalf("recovery=%+v", s.HistoryMigrationStatus())
			}
			if p, err := s.LatestSample("live before crash", "power"); err != nil || p.Value != 123 {
				t.Fatalf("committed live data lost: %+v %v", p, err)
			}
			got, err := s.LoadSeries("meter", "power", 0, 10000, 0)
			if err != nil || len(got) != historyImportRows*2+9 {
				t.Fatalf("SQLite rows=%d %v", len(got), err)
			}
			for _, p := range got {
				if p.Value != float64(p.TsMs)/7 {
					t.Fatalf("SQLite value changed: %+v", p)
				}
			}
			if phase == "parquet" {
				got, err := s.LoadSeries("archive", "power", 10000, 20000, 0)
				if err != nil || len(got) != historyImportRows*2+9 {
					t.Fatalf("archive rows=%d %v", len(got), err)
				}
				for _, p := range got {
					if p.Value != float64(p.TsMs-10000) {
						t.Fatalf("archive value changed: %+v", p)
					}
				}
			}
		})
	}
}

func TestRawRetentionWaitsForHistoryImport(t *testing.T) {
	path, cold := legacyMigrationFixture(t, historyImportRows+7)
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s, err := OpenWithBackgroundHistory(path, cold, func(st HistoryMigrationStatus) {
		if st.Phase == "sqlite" && st.CurrentSourceRowsDone == historyImportRows {
			once.Do(func() { close(reached); <-release })
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var released sync.Once
	defer func() { released.Do(func() { close(release) }); s.Close() }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("import did not reach committed chunk")
	}
	if err := s.PruneHistorySamples(context.Background(), 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&n); err != nil || n != historyImportRows {
		t.Fatalf("retention deleted pending import rows: %d %v", n, err)
	}
	released.Do(func() { close(release) })
	select {
	case <-s.historyMigration.done:
	case <-time.After(10 * time.Second):
		t.Fatal("import did not finish")
	}
	if !s.HistoryMigrationStatus().HistoryComplete {
		t.Fatalf("import failed: %+v", s.HistoryMigrationStatus())
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&n); err != nil || n != historyImportRows+7 {
		t.Fatalf("import lost rows: %d %v", n, err)
	}
	if err := s.PruneHistorySamples(context.Background(), 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("retention did not resume: %d %v", n, err)
	}
}

func TestBackgroundHistoryContinuesBetaOneReceiptsAndSequences(t *testing.T) {
	path, cold := legacyMigrationFixture(t, 17)
	s, err := Open(path) // beta.1 completes SQLite before importing Parquet.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	generation, err := s.historyConfig("history_duckdb_generation")
	if err != nil || generation == "" {
		t.Fatalf("generation=%q %v", generation, err)
	}
	day := filepath.Join(cold, "2026", "01")
	if err := os.MkdirAll(day, 0700); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(day, "01.parquet")
	if err := writeParquetDay(first, []parquetSampleRow{{TsMs: 100, Driver: "meter", Metric: "power", Value: 100}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportLegacyParquet(context.Background(), cold); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(day, "02.parquet")
	points := []parquetSampleRow{{TsMs: 200, Driver: "meter", Metric: "power", Value: 200}, {TsMs: 201, Driver: "meter", Metric: "power", Value: 201}}
	if err := writeParquetDay(second, points); err != nil {
		t.Fatal(err)
	}
	digest, err := historyFileHash(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSamples([]Sample{{TsMs: 200, Driver: "meter", Metric: "power", Value: 999}}); err != nil {
		t.Fatal(err)
	}
	// beta.1 has a bound file hash, but no per-file resume cursor. Its table
	// defaults depend on the seeded sequences; leave that catalog unchanged.
	for _, stmt := range []string{
		`ALTER TABLE ts_drivers ALTER COLUMN id SET DEFAULT nextval('ts_drivers_next_id')`,
		`ALTER TABLE ts_metrics ALTER COLUMN id SET DEFAULT nextval('ts_metrics_next_id')`,
		`DROP TABLE history_parquet_progress`,
		`DROP TABLE history_parquet_manifest`,
		`DROP TABLE history_sqlite_progress`,
	} {
		if _, err := s.history.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.history.Exec(`INSERT INTO history_parquet_imports VALUES (?,?)`, second, digest); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	seedRepeated := false
	s, err = OpenWithBackgroundHistory(path, cold, func(st HistoryMigrationStatus) {
		if st.Phase == "seed" && st.CurrentSource != "" {
			seedRepeated = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.historyMigration.done:
	case <-time.After(10 * time.Second):
		t.Fatal("legacy import did not finish")
	}
	st := s.HistoryMigrationStatus()
	// beta.1 has no saved SQLite row count. Resume must not scan that table
	// just to fill a status counter; only the three known Parquet rows count.
	if seedRepeated || !st.HistoryComplete || st.FilesDone != 2 || st.RowsDone != 3 {
		t.Fatalf("seedRepeated=%v status=%+v", seedRepeated, st)
	}
	if got, err := s.historyConfig("history_duckdb_generation"); err != nil || got != generation {
		t.Fatalf("generation changed: %q %v", got, err)
	}
	if p, err := s.LatestSample("meter", "power"); err != nil || p.Value != 201 {
		t.Fatalf("missing remaining source: %+v %v", p, err)
	}
	got, err := s.LoadSeries("meter", "power", 200, 200, 0)
	if err != nil || len(got) != 1 || got[0].Value != 999 {
		t.Fatalf("overwrote prior primary: %+v %v", got, err)
	}
	if err := s.RecordSamples([]Sample{{TsMs: 300, Driver: "new meter", Metric: "new metric", Value: 123}}); err != nil {
		t.Fatal(err)
	}
	var d, m int64
	if err := s.history.QueryRow(`SELECT driver_id,metric_id FROM ts_samples WHERE ts_ms=300`).Scan(&d, &m); err != nil || d <= 7 || m <= 9 {
		t.Fatalf("reused seeded IDs: %d %d %v", d, m, err)
	}
	if got, err := historyFileHash(second); err != nil || got != digest {
		t.Fatalf("changed original source: %s %v", got, err)
	}
}
