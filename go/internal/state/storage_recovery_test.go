package state

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFreshSQLiteStoreDoesNotCreateBetaFiles(t *testing.T) {
	s := freshStore(t)
	if s.HistoryBackend()["engine"] != "sqlite" {
		t.Fatal(s.HistoryBackend())
	}
	for _, name := range []string{"history.duckdb", "history-hot.db"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(s.mainDBPath), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected %s: %v", name, err)
		}
	}
}

func TestSQLiteInitialBindingResumesAfterInterruptedSave(t *testing.T) {
	for _, n := range []int{0, 2300} {
		for _, background := range []bool{false, true} {
			t.Run(fmt.Sprintf("rows=%d/background=%t", n, background), func(t *testing.T) {
				path, cold := legacyMigrationFixture(t, n)
				cfg, err := openRaw(path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = cfg.Exec(`CREATE TRIGGER interrupt_binding BEFORE INSERT ON config
					WHEN NEW.key='history_sqlite_generation' BEGIN SELECT RAISE(ABORT,'interrupted binding'); END`)
				cfg.Close()
				if err != nil {
					t.Fatal(err)
				}
				open := func() (*Store, error) {
					if background {
						return OpenWithBackgroundHistory(path, cold, nil)
					}
					return Open(path)
				}
				if st, err := open(); err == nil {
					st.Close()
					t.Fatal("binding unexpectedly completed")
				} else if !strings.Contains(err.Error(), "interrupted binding") {
					t.Fatal(err)
				}
				cfg, err = openRaw(path)
				if err != nil {
					t.Fatal(err)
				}
				_, err = cfg.Exec(`DROP TRIGGER interrupt_binding`)
				cfg.Close()
				if err != nil {
					t.Fatal(err)
				}
				st, err := open()
				if err != nil {
					t.Fatalf("restart must finish its own binding: %v", err)
				}
				defer st.Close()
				if background {
					waitHistoryMigration(t, st)
				}
				got, err := st.LoadSeries("meter", "power", 1, 2300, 0)
				if err != nil || len(got) != n {
					t.Fatalf("restart lost history: rows=%d err=%v", len(got), err)
				}
				var generation string
				if err := st.history.QueryRow(`SELECT name FROM history_migrations WHERE name LIKE 'generation:%'`).Scan(&generation); err != nil {
					t.Fatal(err)
				}
				active, _ := st.historyConfig("history_sqlite_generation")
				pending, _ := st.historyConfig("history_sqlite_pending_generation")
				if active == "" || generation != "generation:"+active || pending != "" {
					t.Fatalf("binding not finished: active=%q generation=%q pending=%q", active, generation, pending)
				}
			})
		}
	}
}

func TestSQLiteLegacyResumeRetainsParquetIdentityAndLiveTicks(t *testing.T) {
	path, cold := legacyMigrationFixture(t, 5000)
	day := time.Now().AddDate(0, 0, -90).UTC().Truncate(24 * time.Hour)
	dir := filepath.Join(cold, day.Format("2006/01"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pq := filepath.Join(dir, day.Format("02.parquet"))
	if err := writeParquetDay(pq, []parquetSampleRow{{TsMs: day.UnixMilli(), Driver: "meter", Metric: "power", Value: 99}}); err != nil {
		t.Fatal(err)
	}
	before, err := historyFileHash(pq)
	if err != nil {
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
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("import never reached a committed chunk")
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 6000, Driver: "meter", Metric: "power", Value: 6}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	s.historyMigration.cancel()
	close(release)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenWithBackgroundHistory(path, cold, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitHistoryMigration(t, s)
	if !s.HistoryMigrationStatus().HistoryComplete {
		t.Fatal(s.HistoryMigrationStatus())
	}
	var id int64
	if err := s.history.QueryRow(`SELECT id FROM ts_drivers WHERE name='meter'`).Scan(&id); err != nil || id != 7 {
		t.Fatalf("identity changed: %d %v", id, err)
	}
	got, err := s.LoadSeries("meter", "power", 1, 6000, 0)
	if err != nil || len(got) != 5001 {
		t.Fatalf("resume dropped samples: %d %v", len(got), err)
	}
	got, err = s.LoadSeries("meter", "power", day.UnixMilli(), day.Add(time.Hour).UnixMilli(), 0)
	if err != nil || len(got) != 1 || got[0].Value != 99 {
		t.Fatalf("legacy Parquet unavailable: %v %v", got, err)
	}
	after, err := historyFileHash(pq)
	if err != nil || before != after {
		t.Fatal("legacy Parquet changed", err)
	}
	if n := sqliteLegacyHistoryTableCount(s); n != len(historyTables) {
		t.Fatal("legacy SQLite source deleted", n)
	}
}

func TestArchiveRetryKeepsAllRowsPeaksAndOlderSummaries(t *testing.T) {
	s := freshStore(t)
	s.coldDir = t.TempDir()
	ctx := context.Background()
	day := time.Now().AddDate(0, 0, -40).UTC().Truncate(24 * time.Hour)
	put := func(ts int64, v float64) {
		t.Helper()
		if err := s.RecordSamples([]Sample{{Driver: "ev", Metric: "power", TsMs: ts, Value: v}}); err != nil {
			t.Fatal(err)
		}
	}
	put(day.UnixMilli(), 0)
	put(day.Add(time.Minute).UnixMilli(), 11000)
	if _, err := s.history.Exec(`CREATE TRIGGER fail_archive_prune BEFORE DELETE ON ts_samples BEGIN SELECT RAISE(ABORT,'injected prune failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RolloffToParquet(ctx, s.coldDir); err == nil {
		t.Fatal("prune fault ignored")
	}
	put(day.Add(2*time.Minute).UnixMilli(), 1000)
	if _, err := s.history.Exec(`DROP TRIGGER fail_archive_prune`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RolloffToParquet(ctx, s.coldDir); err != nil {
		t.Fatal(err)
	}
	pts, err := s.LoadSeries("ev", "power", day.UnixMilli(), day.Add(time.Hour).UnixMilli(), 0)
	if err != nil || len(pts) != 3 {
		t.Fatalf("retry rows=%v %v", pts, err)
	}
	if err := s.PruneHistorySamples(ctx, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	paths, err := parquetPaths(s.coldDir, day.UnixMilli(), day.Add(time.Hour).UnixMilli())
	if err != nil || len(paths) != 0 {
		t.Fatal(paths, err)
	}
	buckets, err := s.LoadSeriesBuckets("ev", "power", day.UnixMilli(), day.Add(24*time.Hour).UnixMilli()-1, 24)
	if err != nil || len(buckets) != 1 || buckets[0].N != 3 || buckets[0].Min != 0 || buckets[0].Max != 11000 || buckets[0].V != 4000 {
		t.Fatalf("lost old envelope: %+v %v", buckets, err)
	}
	// A late, unique point after raw expiry must add to the durable total.
	put(day.Add(3*time.Minute).UnixMilli(), 1000)
	if err := s.PruneHistorySamples(ctx, 30, time.Now()); err != nil {
		t.Fatal(err)
	}
	buckets, err = s.LoadSeriesBuckets("ev", "power", day.UnixMilli(), day.Add(24*time.Hour).UnixMilli()-1, 24)
	if err != nil || len(buckets) != 1 || buckets[0].N != 4 || buckets[0].V != 3250 || buckets[0].Max != 11000 {
		t.Fatalf("late point erased old total: %+v %v", buckets, err)
	}

}

func TestArchiveRejectsCorruptionBeforeDeletingSQLite(t *testing.T) {
	s := freshStore(t)
	s.coldDir = t.TempDir()
	day := time.Now().AddDate(0, 0, -40).UTC().Truncate(24 * time.Hour)
	if err := s.RecordSamples([]Sample{{Driver: "ev", Metric: "power", TsMs: day.UnixMilli(), Value: 7000}}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.coldDir, day.Format("2006/01"))
	os.MkdirAll(dir, 0700)
	if err := os.WriteFile(filepath.Join(dir, day.Format("02.parquet")), []byte("truncated archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RolloffToParquet(context.Background(), s.coldDir); err == nil {
		t.Fatal("accepted corrupt archive")
	}
	var n int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&n); err != nil || n != 1 {
		t.Fatal("deleted unarchived rows", n, err)
	}
}

func TestHistoryReaderAndArchiveLockDoNotBlockGoalsOrEnergyCommit(t *testing.T) {
	s := freshStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.RecordSamples([]Sample{{Driver: "ev", Metric: "power", TsMs: 1, Value: 7000}}); err != nil {
		t.Fatal(err)
	}
	reader, err := s.history.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var n int
	if err := reader.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	started := time.Now()
	if err := s.SaveConfig("ev_session:test", `{"estimated_wh":12345}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("charging_goal", `{"soc":0.8,"time":"07:00"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(nil, []Sample{{Driver: "ev", Metric: "power", TsMs: 2, Value: 7100}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("read snapshot or archive lock blocked live work")
	}
	if s.HistoryWriterStatus().Committed != 1 {
		t.Fatal(s.HistoryWriterStatus())
	}
}

func betaSQLiteFixture(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := s.historyConfig("history_sqlite_generation")
	if err != nil {
		t.Fatal(err)
	}
	var samples []Sample
	for i := 1; i <= 2300; i++ {
		samples = append(samples, Sample{Driver: "meter", Metric: "power", TsMs: int64(i), Value: float64(i)})
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("saved_goal", "80% by 07:00"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM config WHERE key='history_sqlite_generation'; INSERT INTO config VALUES('history_duckdb_generation',?)`, generation); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(filepath.Dir(path), "history.duckdb")
	if err := os.Rename(historyDatabasePath(path), old); err != nil {
		t.Fatal(err)
	}
	source, err := sql.Open("sqlite", ReadOnlyDatabaseURI(old))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	return path, source
}

func TestBetaConversionKeepsOriginalsAndResumesAfterPublish(t *testing.T) {
	// This fixture exercises the engine-independent copy/recovery contract.
	// The separate converter module repeats it with a real DuckDB source.
	path, source := betaSQLiteFixture(t)
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("silently opened beta data without conversion")
	}
	original := filepath.Join(filepath.Dir(path), "history.duckdb")
	before, err := historyFileHash(original)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = ConvertBetaHistory(ctx, path, source, func(phase string) {
		if phase == "published verified history" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interrupted bind: %v", err)
	}
	if err := ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal(err)
	}
	after, err := historyFileHash(original)
	if err != nil || before != after {
		t.Fatal("beta original changed", err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	pts, err := s.LoadSeries("meter", "power", 1, 2300, 0)
	if err != nil || len(pts) != 2300 {
		t.Fatalf("converted data=%d %v", len(pts), err)
	}
	if got, _ := s.LoadConfig("saved_goal"); got != "80% by 07:00" {
		t.Fatal("goal changed", got)
	}
}

func TestQueryLimitsRejectBeforeHugeOutput(t *testing.T) {
	s := freshStore(t)
	if _, err := s.LoadSeriesBuckets("ev", "power", 0, 10000, maxSeriesBuckets+1); !errors.Is(err, ErrHistoryQueryLimit) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.LoadSeriesContext(ctx, "ev", "power", 0, 1<<62, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestArchiveSummaryIncludesLateRawPointOnce(t *testing.T) {
	s := freshStore(t)
	stage, err := openArchiveStage(filepath.Join(t.TempDir(), "stage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	ctx := context.Background()
	if err := insertArchiveRows(ctx, stage, []parquetSampleRow{{TsMs: 1, Driver: "ev", Metric: "power", Value: 100}, {TsMs: 2, Driver: "ev", Metric: "power", Value: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSamples([]Sample{{TsMs: 2, Driver: "ev", Metric: "power", Value: 200}, {TsMs: 3, Driver: "ev", Metric: "power", Value: 900}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.archiveDayHours(ctx, stage); err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	var sum, peak float64
	if err := s.history.QueryRow(`SELECT n,sum_value,max_value FROM ts_series_hour`).Scan(&n, &sum, &peak); err != nil {
		t.Fatal(err)
	}
	if n != 3 || sum != 1200 || peak != 900 {
		t.Fatalf("summary lost/duplicated late sample: %d %v %v", n, sum, peak)
	}
}

func TestFirstLiveTickBeforeConvertedHourBackfill(t *testing.T) {
	s := freshStore(t)
	if err := s.RecordSamples([]Sample{{TsMs: 1, Driver: "ev", Metric: "power", Value: 100}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`DELETE FROM ts_series_hour`); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSamples([]Sample{{TsMs: 2, Driver: "ev", Metric: "power", Value: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureSeriesHours(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	var sum float64
	if err := s.history.QueryRow(`SELECT n,sum_value FROM ts_series_hour`).Scan(&n, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 2 || sum != 300 {
		t.Fatalf("live write hid converted history: %d %v", n, sum)
	}
}

func TestOfflineBackupRefusesUnconvertedBeta(t *testing.T) {
	path, _ := betaSQLiteFixture(t)
	s, err := OpenBackupSource(path)
	if err == nil {
		s.Close()
		t.Fatal("backup silently used frozen SQLite rows")
	}
	if !strings.Contains(err.Error(), "convert beta history") {
		t.Fatal(err)
	}
}

func TestSQLiteFullHistoryDiskKeepsGoalAndRetriesWholeTick(t *testing.T) {
	s := freshStore(t)
	s.history.SetMaxOpenConns(1)
	if err := s.RecordSamples([]Sample{{TsMs: 1, Driver: "ev", Metric: "power", Value: 7000}}); err != nil {
		t.Fatal(err)
	}
	var pages int
	if err := s.history.QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(fmt.Sprintf(`PRAGMA max_page_count=%d`, pages+2)); err != nil {
		t.Fatal(err)
	}
	s.historyWriter.commitInterval = time.Hour
	p := HistoryPoint{TsMs: 2, JSON: `{"data":"` + strings.Repeat("x", 128<<10) + `"}`}
	if err := s.EnqueueTelemetryTick(&p, []Sample{{TsMs: 2, Driver: "ev", Metric: "power", Value: 7100}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err := s.FlushHistory(ctx)
	cancel()
	if err == nil {
		t.Fatal("full database unexpectedly committed")
	}
	if s.HistoryWriterStatus().Committed != 0 {
		t.Fatal("partial tick acknowledged")
	}
	if err := s.SaveConfig("ev_goal", "80% by 07:00"); err != nil {
		t.Fatal("history failure blocked durable goal", err)
	}
	if _, err := s.history.Exec(`PRAGMA max_page_count=2147483646`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if s.HistoryWriterStatus().Committed != 1 {
		t.Fatal(s.HistoryWriterStatus())
	}
	pts, err := s.LoadSeries("ev", "power", 0, 3, 0)
	if err != nil || len(pts) != 2 {
		t.Fatalf("retry lost or duplicated samples: %v %v", pts, err)
	}
	if got, _ := s.LoadConfig("ev_goal"); got != "80% by 07:00" {
		t.Fatal("goal lost", got)
	}
}

func TestHistoryCrashHelper(t *testing.T) {
	path := os.Getenv("FTW_STORAGE_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConfig("ev_session", `{"soc":0.8021,"goal":0.8,"wh":13081}`); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueTelemetryTick(&HistoryPoint{TsMs: 123, JSON: "{}"}, []Sample{{TsMs: 123, Driver: "ev", Metric: "power", Value: 0}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	fmt.Println("STORAGE_COMMITTED")
	<-time.After(time.Minute)
}

func TestHistorySurvivesProcessKillAfterCommit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHistoryCrashHelper$")
	cmd.Env = append(os.Environ(), "FTW_STORAGE_CRASH_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	ready := false
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		if lines.Text() == "STORAGE_COMMITTED" {
			ready = true
			break
		}
	}
	if !ready {
		cmd.Wait()
		t.Fatal("helper never committed", ctx.Err(), lines.Err())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("helper exited cleanly")
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, _ := s.LoadConfig("ev_session"); got != `{"soc":0.8021,"goal":0.8,"wh":13081}` {
		t.Fatal("session lost", got)
	}
	pts, err := s.LoadSeries("ev", "power", 0, 200, 0)
	if err != nil || len(pts) != 1 || pts[0].Value != 0 {
		t.Fatalf("committed telemetry lost: %v %v", pts, err)
	}
}

func TestBetaHotLedgerRetryKeepsCounterEnergyOnce(t *testing.T) {
	s := freshStore(t)
	ctx := context.Background()
	base := int64(1_800_000_000_000)
	asset := HardwareEnergyAssetID("easee:stable", AssetVehicleCharger)
	var observations []EnergyObservation
	for i, c := range []float64{100, 200, 300} {
		observations = append(observations, ledgerObservation(asset, AssetVehicleCharger, FlowVehicleCharge, base+int64(i)*60_000, energyPtr(c), energyPtr(6000)))
	}
	for _, o := range observations[:2] {
		recordEnergyTestTick(t, s, o.AtMs, o)
	}
	hot, err := openRaw(filepath.Join(t.TempDir(), "hot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hot.Close()
	if err := ensureSqliteLegacyHistory(func(q string) error { _, err := hot.Exec(q); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := hot.Exec(`CREATE TABLE hot_ticks(id TEXT,ts_ms INTEGER,payload TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.history.Exec(`CREATE TABLE conversion_progress(name TEXT PRIMARY KEY,cursor TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for i, o := range observations {
		payload, err := json.Marshal(historyPayload{Observations: []EnergyObservation{o}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hot.Exec(`INSERT INTO hot_ticks VALUES(?,?,?)`, fmt.Sprint(i), o.AtMs, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := mergeBetaHotHistory(ctx, hot, s.history); err != nil {
			t.Fatal(err)
		}
		// Simulate loss of the phase marker after durable row commits.
		if _, err := s.history.Exec(`DELETE FROM conversion_progress WHERE name='hot:ledger:complete'`); err != nil {
			t.Fatal(err)
		}
	}
	var total float64
	for _, p := range loadLedgerTestPoints(t, s, asset, base, base+180_000) {
		total += p.EnergyWh
	}
	if total != 200 {
		t.Fatalf("counter energy duplicated/lost: %v", total)
	}
}

func TestHistoryCommitSyncsWAL(t *testing.T) {
	s := freshStore(t)
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		c, err := s.history.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		var sync int
		if err := c.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&sync); err != nil {
			t.Fatal(err)
		}
		if sync != 2 {
			t.Fatalf("history acknowledged before WAL sync: %d", sync)
		}
	}
}

func writeParquetDay(path string, rows []parquetSampleRow) error {
	stagePath := path + ".fixture.db"
	stage, err := openArchiveStage(stagePath)
	if err != nil {
		return err
	}
	defer os.Remove(stagePath)
	defer stage.Close()
	if err := insertArchiveRows(context.Background(), stage, rows); err != nil {
		return err
	}
	return publishStagedSamples(context.Background(), path, stage)
}

// Opt-in admission fixture: records kernel peak RSS on the target. Also use
// an enforced memory limit when the host kernel supports one.
func TestStorageAdmissionDurableGoalsWithoutBackup(t *testing.T) {
	if os.Getenv("FTW_STORAGE_ADMISSION") != "1" {
		t.Skip("set FTW_STORAGE_ADMISSION=1 for the target IO baseline")
	}
	s := freshStore(t)
	defer checkStorageAdmissionRSS(t)
	s.historyWriter.commitInterval = 20 * time.Millisecond
	var worst time.Duration
	for i := 0; i < 200; i++ {
		start := time.Now()
		if err := s.SaveConfig("ev_goal", fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
		worst = max(worst, time.Since(start))
		if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: int64(i + 1), Driver: "ev", Metric: "power", Value: 7000}}, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("without backup: max durable goal save %v, history %+v", worst, s.HistoryWriterStatus())
	if worst > 2*time.Second {
		t.Fatalf("durable goal baseline exceeds latency limit: %v", worst)
	}
}

func TestStorageAdmissionBackupRestoreWithLiveWrites(t *testing.T) {
	if os.Getenv("FTW_STORAGE_ADMISSION") != "1" {
		t.Skip("set FTW_STORAGE_ADMISSION=1 for the 48 MiB backup fixture")
	}
	s := freshStore(t)
	defer checkStorageAdmissionRSS(t)
	payload := `{"samples":"` + strings.Repeat("abcdefgh01234567", 1024) + `"}`
	points := make([]HistoryPoint, 3000)
	for i := range points {
		points[i] = HistoryPoint{TsMs: int64(i + 1), GridW: float64(i), JSON: payload}
	}
	if err := s.BulkRecordHistory(points); err != nil {
		t.Fatal(err)
	}
	points = nil
	if err := s.SaveConfig("ev_goal", "80% by 07:00"); err != nil {
		t.Fatal(err)
	}
	s.historyWriter.commitInterval = 20 * time.Millisecond
	backup := filepath.Join(t.TempDir(), "full.gz")
	done := make(chan error, 1)
	go func() {
		done <- s.BackupToCompressedWithProgress(backup, func(p BackupProgress) {
			t.Logf("backup phase=%s bytes=%d/%d", p.Phase, p.CompletedBytes, p.TotalBytes)
		})
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var writes int
	var worst time.Duration
loop:
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			break loop
		case <-ticker.C:
			start := time.Now()
			if err := s.SaveConfig("admission_check", fmt.Sprint(writes)); err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed > worst {
				worst = elapsed
				if elapsed > 500*time.Millisecond {
					t.Logf("slow durable goal save: %v at live write %d", elapsed, writes)
				}
			}
			writes++
			if err := s.EnqueueTelemetryTick(nil, []Sample{{TsMs: 100000 + int64(writes), Driver: "ev", Metric: "power", Value: 7000}}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.FlushHistory(ctx); err != nil {
		t.Fatal(err)
	}
	status := s.HistoryWriterStatus()
	if writes == 0 || status.Rejected != 0 || status.Committed != uint64(writes) {
		t.Fatalf("backup interrupted telemetry: writes=%d status=%+v", writes, status)
	}
	if worst > 2*time.Second {
		t.Fatalf("backup blocked settings for %v", worst)
	}
	f, err := os.Open(backup)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	path := filepath.Join(t.TempDir(), "state.db")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, gz)
	closeErr := out.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var n, bytes int64
	var sum float64
	if err := restored.history.QueryRow(`SELECT COUNT(*),SUM(length(json)),SUM(grid_w) FROM history_hot`).Scan(&n, &bytes, &sum); err != nil {
		t.Fatal(err)
	}
	if n != 3000 || bytes != 3000*int64(len(payload)) || sum != 4498500 {
		t.Fatalf("backup differs: %d %d %v", n, bytes, sum)
	}
	if goal, _ := restored.LoadConfig("ev_goal"); goal != "80% by 07:00" {
		t.Fatal("restored goal differs", goal)
	}
	// Wide rows must not make either a chart or a raw export exceed its budget.
	if pts, err := s.LoadHistory(0, 4000, 64); err != nil || len(pts) > 64 {
		t.Fatalf("bounded chart: %d %v", len(pts), err)
	}
	if _, err := s.LoadHistory(0, 4000, 0); !errors.Is(err, ErrHistoryQueryLimit) {
		t.Fatal("raw JSON output was unbounded", err)
	}
	t.Logf("verified %d snapshot rows, %d live commits, max durable setting write %v", n, writes, worst)
}

func TestBetaConversionRejectsMalformedLegacySample(t *testing.T) {
	s := freshStore(t)
	legacy, err := openRaw(filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(`CREATE TABLE ts_samples(driver_id INTEGER,metric_id INTEGER,ts_ms INTEGER,value REAL); INSERT INTO ts_samples VALUES(1,1,1,'broken')`); err != nil {
		t.Fatal(err)
	}
	err = mergeLegacyConversionTable(context.Background(), legacy, s.history, "ts_samples")
	if err == nil || !strings.Contains(err.Error(), "invalid value types") {
		t.Fatalf("malformed source must fail without panic: %v", err)
	}
	var count int
	if err := s.history.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&count); err != nil || count != 0 {
		t.Fatal("malformed source changed destination", count, err)
	}
}

func TestBetaConversionResumesBeforePublish(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	err := ConvertBetaHistory(ctx, path, source, func(phase string) {
		if phase == "copy and verify energy_daily" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expected interrupted copy", err)
	}
	if err := ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal("resume", err)
	}
}

func TestBetaConversionRefusesChangedSourceOnResume(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	err := ConvertBetaHistory(ctx, path, source, func(phase string) {
		if phase == "copy and verify energy_daily" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cfg, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cfg.Exec(`UPDATE config SET value='new source state' WHERE key='saved_goal'`)
	cfg.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := ConvertBetaHistory(context.Background(), path, source, nil); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatal("mixed different sources", err)
	}
}

func TestBetaConversionPreservesUncheckpointedStateWALOnCancel(t *testing.T) {
	path, source := betaSQLiteFixture(t)
	cfg, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Exec(`PRAGMA wal_autocheckpoint=0; UPDATE config SET value='90% by 08:00' WHERE key='saved_goal'`); err != nil {
		t.Fatal(err)
	}
	mainBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	walBytes, err := os.ReadFile(path + "-wal")
	if err != nil || len(walBytes) <= 32 {
		t.Fatal("fixture must have committed WAL frames", len(walBytes), err)
	}
	if err := cfg.Close(); err != nil {
		t.Fatal(err)
	}
	// Restore the frozen main/WAL pair, as left by an exited process. No
	// other connection may checkpoint it while the converter runs.
	for name, data := range map[string][]byte{path: mainBytes, path + "-wal": walBytes} {
		if err := os.WriteFile(name, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	err = ConvertBetaHistory(ctx, path, source, func(phase string) {
		if phase == "copy and verify energy_daily" {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("expected interrupted copy", err)
	}
	for name, expected := range map[string][]byte{path: mainBytes, path + "-wal": walBytes} {
		got, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatal("interrupted converter changed its source", filepath.Base(name), err)
		}
	}
	if err := ConvertBetaHistory(context.Background(), path, source, nil); err != nil {
		t.Fatal("resume from frozen WAL", err)
	}
	cfg, err = sql.Open("sqlite", ReadOnlyDatabaseURI(path))
	if err != nil {
		t.Fatal(err)
	}
	defer cfg.Close()
	var goal string
	if err := cfg.QueryRow(`SELECT value FROM config WHERE key='saved_goal'`).Scan(&goal); err != nil || goal != "90% by 08:00" {
		t.Fatal("committed WAL goal changed", goal, err)
	}
}

func TestBetaFingerprintIgnoresOnlyEmptySQLiteWAL(t *testing.T) {
	path, _ := betaSQLiteFixture(t)
	ctx := context.Background()
	before, err := betaSourceHash(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, wal := range []string{path + "-wal", hotHistoryPath(path) + "-wal"} {
		if err := os.WriteFile(wal, nil, 0600); err != nil {
			t.Fatal(err)
		}
		got, err := betaSourceHash(ctx, path)
		if err != nil || got != before {
			t.Fatal("empty reader WAL changed source fingerprint", err)
		}
		if err := os.WriteFile(wal, []byte("nonempty WAL must remain protected"), 0600); err != nil {
			t.Fatal(err)
		}
		got, err = betaSourceHash(ctx, path)
		if err != nil || got == before {
			t.Fatal("nonempty WAL was not fingerprinted", err)
		}
		if err := os.Remove(wal); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCloseCancelsBlockedSeriesBackfill(t *testing.T) {
	s := freshStore(t)
	s.coldDir = t.TempDir()
	dir := filepath.Join(s.coldDir, "2026", "01")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeParquetDay(filepath.Join(dir, "01.parquet"), []parquetSampleRow{{TsMs: 1767225600000, Driver: "ev", Metric: "power", Value: 7000}}); err != nil {
		t.Fatal(err)
	}
	s.archiveMu.Lock()
	defer s.archiveMu.Unlock()
	s.startSeriesHourBackfill()
	done := make(chan error, 1)
	go func() { done <- s.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown waited for archive backfill")
	}
}

func TestInterruptedArchiveCleanupKeepsSourcesAndActiveTemps(t *testing.T) {
	dir := t.TempDir()
	month := filepath.Join(dir, "2026", "01")
	if err := os.MkdirAll(month, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	for _, name := range []string{".ftw-samples-old.db", ".ftw-samples-active.db", "01.parquet"} {
		path := filepath.Join(month, name)
		if err := os.WriteFile(path, []byte("source"), 0600); err != nil {
			t.Fatal(err)
		}
		if name != ".ftw-samples-active.db" {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := cleanupArchiveTemps(context.Background(), dir, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(month, ".ftw-samples-old.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan remains", err)
	}
	for _, name := range []string{".ftw-samples-active.db", "01.parquet"} {
		if _, err := os.Stat(filepath.Join(month, name)); err != nil {
			t.Fatal("removed retained file", name, err)
		}
	}
}

func TestBetaOpenRefusalDoesNotChangeStateSource(t *testing.T) {
	path, _ := betaSQLiteFixture(t)
	before, err := historyFileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("unconverted beta opened")
	}
	after, err := historyFileHash(path)
	if err != nil || before != after {
		t.Fatal("failed startup changed conversion source", err)
	}
}
