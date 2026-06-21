package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func freshStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTimeSeriesInternCacheIsPerStore(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(filepath.Join(dir, "one.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.RecordSamples([]Sample{{Driver: "driver", Metric: "metric_w", TsMs: 1, Value: 10}}); err != nil {
		t.Fatalf("record first store: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	secondPath := filepath.Join(dir, "two.db")
	s2, err := Open(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.RecordSamples([]Sample{{Driver: "driver", Metric: "metric_w", TsMs: 2, Value: 20}}); err != nil {
		t.Fatalf("record second store: %v", err)
	}
	var driverRows, metricRows int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM ts_drivers`).Scan(&driverRows); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM ts_metrics`).Scan(&metricRows); err != nil {
		t.Fatal(err)
	}
	if driverRows != 1 || metricRows != 1 {
		t.Fatalf("second store intern rows: drivers=%d metrics=%d, want 1/1", driverRows, metricRows)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("close second store: %v", err)
	}

	reopened, err := Open(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	series, err := reopened.LoadSeries("driver", "metric_w", 0, 10, 0)
	if err != nil {
		t.Fatalf("load reopened series: %v", err)
	}
	if len(series) != 1 || series[0].TsMs != 2 || series[0].Value != 20 {
		t.Fatalf("reopened series = %+v, want one persisted sample", series)
	}
}

func TestConfigRoundtrip(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveConfig("mode", "self_consumption"); err != nil {
		t.Fatal(err)
	}
	v, ok := s.LoadConfig("mode")
	if !ok || v != "self_consumption" {
		t.Errorf("mode: got %q ok=%v", v, ok)
	}
	// Upsert
	if err := s.SaveConfig("mode", "charge"); err != nil {
		t.Fatal(err)
	}
	v, _ = s.LoadConfig("mode")
	if v != "charge" {
		t.Errorf("after upsert: got %q", v)
	}
	if _, ok := s.LoadConfig("missing"); ok {
		t.Error("missing key should not return ok")
	}
}

// 2026-05-25 performance regression: /api/energy/daily?days=30
// cold-started at ~25 s on a 1 GB state.db because every closed day
// re-ran a per-day DailyEnergy SQL pass. SaveDailyEnergy +
// LoadDailyEnergy persist the aggregate so the same call after
// restart resolves to N PK lookups instead.
func TestDailyEnergyPersistRoundtrip(t *testing.T) {
	s := freshStore(t)
	de := DayEnergy{
		ImportWh:        1234.5,
		ExportWh:        678.9,
		PVWh:            5000,
		BatChargedWh:    1500,
		BatDischargedWh: 1100,
		LoadWh:          2222,
	}
	if err := s.SaveDailyEnergy("2026-05-25", de); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := s.LoadDailyEnergy("2026-05-25")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true after save")
	}
	if got != de {
		t.Errorf("roundtrip mismatch: got %+v, want %+v", got, de)
	}
	// Upsert with new values must overwrite, not append a duplicate.
	de2 := de
	de2.ImportWh = 9999
	if err := s.SaveDailyEnergy("2026-05-25", de2); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got2, _, _ := s.LoadDailyEnergy("2026-05-25")
	if got2.ImportWh != 9999 {
		t.Errorf("upsert did not overwrite: got %f", got2.ImportWh)
	}
}

func TestDailyEnergyMissReturnsFalse(t *testing.T) {
	s := freshStore(t)
	_, ok, err := s.LoadDailyEnergy("1999-01-01")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Error("ok should be false for a day never persisted")
	}
}

// DailyEnergy.Intervals reports the number of integration intervals
// (rows with a predecessor). Right after midnight with 0–1 rows there
// is nothing to integrate, so callers must be able to tell that apart
// from a genuine zero — Intervals carries that signal.
func TestDailyEnergyIntervalsDistinguishesNoDataFromZero(t *testing.T) {
	base := time.Date(2026, 5, 23, 0, 0, 0, 0, time.Local)

	t.Run("empty range", func(t *testing.T) {
		s := freshStore(t)
		d, err := s.DailyEnergy(base.UnixMilli(), base.Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatalf("DailyEnergy: %v", err)
		}
		if d.Intervals != 0 {
			t.Errorf("Intervals = %d, want 0 (no rows)", d.Intervals)
		}
	})

	t.Run("single row", func(t *testing.T) {
		s := freshStore(t)
		if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(time.Minute).UnixMilli(), GridW: 1200}); err != nil {
			t.Fatalf("RecordHistory: %v", err)
		}
		d, err := s.DailyEnergy(base.UnixMilli(), base.Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatalf("DailyEnergy: %v", err)
		}
		// One row has no predecessor → no interval, even though a row exists.
		if d.Intervals != 0 {
			t.Errorf("Intervals = %d, want 0 (single row, nothing to integrate)", d.Intervals)
		}
		if d.ImportWh != 0 {
			t.Errorf("ImportWh = %v, want 0 (no interval)", d.ImportWh)
		}
	})

	t.Run("multiple rows", func(t *testing.T) {
		s := freshStore(t)
		if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(1 * time.Minute).UnixMilli(), GridW: 1200}); err != nil {
			t.Fatalf("RecordHistory 1: %v", err)
		}
		if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(4 * time.Minute).UnixMilli(), GridW: 1200}); err != nil {
			t.Fatalf("RecordHistory 2: %v", err)
		}
		if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(6 * time.Minute).UnixMilli(), GridW: 1200}); err != nil {
			t.Fatalf("RecordHistory 3: %v", err)
		}
		d, err := s.DailyEnergy(base.UnixMilli(), base.Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatalf("DailyEnergy: %v", err)
		}
		// 3 rows → 2 intervals (rows 2 and 3 each have a predecessor).
		if d.Intervals != 2 {
			t.Errorf("Intervals = %d, want 2 (3 rows → 2 intervals)", d.Intervals)
		}
		if d.ImportWh <= 0 {
			t.Errorf("ImportWh = %v, want > 0 (intervals integrated)", d.ImportWh)
		}
	})
}

func TestConfigPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.SaveConfig("greeting", "hello")
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	v, ok := s2.LoadConfig("greeting")
	if !ok || v != "hello" {
		t.Errorf("persistence: got %q ok=%v", v, ok)
	}
}

func TestEventsRecorded(t *testing.T) {
	s := freshStore(t)
	for i := 0; i < 5; i++ {
		if err := s.RecordEvent("evt"); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.RecentEvents(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 1 {
		t.Errorf("expected ≥1 events, got %d", len(events))
	}
}

func TestSamplesBeforeKeepsSameTimestampAcrossBatches(t *testing.T) {
	s := freshStore(t)
	samples := []Sample{
		{Driver: "a", Metric: "power_w", TsMs: 10, Value: 1},
		{Driver: "b", Metric: "power_w", TsMs: 10, Value: 2},
		{Driver: "c", Metric: "power_w", TsMs: 10, Value: 3},
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatalf("record samples: %v", err)
	}

	var got []Sample
	err := s.SamplesBefore(context.Background(), 11, 2, func(batch []Sample) error {
		got = append(got, batch...)
		return nil
	})
	if err != nil {
		t.Fatalf("SamplesBefore: %v", err)
	}
	if len(got) != len(samples) {
		t.Fatalf("SamplesBefore returned %d samples, want %d: %+v", len(got), len(samples), got)
	}
	seen := map[string]bool{}
	for _, sm := range got {
		seen[sm.Driver] = true
	}
	for _, driver := range []string{"a", "b", "c"} {
		if !seen[driver] {
			t.Fatalf("SamplesBefore skipped driver %q from shared timestamp batch: %+v", driver, got)
		}
	}
}

func TestLoadSeriesDownsamplingIncludesLatestSample(t *testing.T) {
	s := freshStore(t)
	samples := make([]Sample, 0, 10)
	for i := 0; i < 10; i++ {
		samples = append(samples, Sample{
			Driver: "meter",
			Metric: "power_w",
			TsMs:   int64(i),
			Value:  float64(i),
		})
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatalf("record samples: %v", err)
	}
	got, err := s.LoadSeries("meter", "power_w", 0, 9, 3)
	if err != nil {
		t.Fatalf("LoadSeries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("LoadSeries returned %d points, want 3: %+v", len(got), got)
	}
	if got[0].TsMs != 0 || got[len(got)-1].TsMs != 9 {
		t.Fatalf("downsampled series endpoints = %d..%d, want 0..9: %+v", got[0].TsMs, got[len(got)-1].TsMs, got)
	}
}

func TestRolloffToParquetPreservesExistingDayRows(t *testing.T) {
	s := freshStore(t)
	coldDir := t.TempDir()
	day := time.Now().Add(-RecentRetention - 48*time.Hour).UTC().Truncate(24 * time.Hour)
	first := day.Add(10 * time.Hour).UnixMilli()
	second := day.Add(11 * time.Hour).UnixMilli()

	if err := s.RecordSamples([]Sample{
		{Driver: "meter", Metric: "grid_w", TsMs: first, Value: 100},
	}); err != nil {
		t.Fatalf("record first sample: %v", err)
	}
	if rows, _, err := s.RolloffToParquet(context.Background(), coldDir); err != nil {
		t.Fatalf("first rolloff: %v", err)
	} else if rows != 1 {
		t.Fatalf("first rolloff rows = %d, want 1", rows)
	}

	if err := s.RecordSamples([]Sample{
		{Driver: "meter", Metric: "grid_w", TsMs: second, Value: 200},
	}); err != nil {
		t.Fatalf("record second sample: %v", err)
	}
	if rows, _, err := s.RolloffToParquet(context.Background(), coldDir); err != nil {
		t.Fatalf("second rolloff: %v", err)
	} else if rows != 1 {
		t.Fatalf("second rolloff rows = %d, want 1", rows)
	}

	got, err := s.LoadSeriesFromParquet(coldDir, "meter", "grid_w", first, second)
	if err != nil {
		t.Fatalf("LoadSeriesFromParquet: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("cold series len = %d, want 2: %+v", len(got), got)
	}
	if got[0].TsMs != first || got[0].Value != 100 || got[1].TsMs != second || got[1].Value != 200 {
		t.Fatalf("cold series = %+v, want first+second preserved", got)
	}
}

func TestBatteryModelStore(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveBatteryModel("ferroamp", `{"a":0.7,"b":0.3}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBatteryModel("sungrow", `{"a":0.5,"b":0.4}`); err != nil {
		t.Fatal(err)
	}
	all, err := s.LoadAllBatteryModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 models, got %d", len(all))
	}
	if all["ferroamp"] != `{"a":0.7,"b":0.3}` {
		t.Errorf("ferroamp: %s", all["ferroamp"])
	}
	if err := s.DeleteBatteryModel("sungrow"); err != nil {
		t.Fatal(err)
	}
	all, _ = s.LoadAllBatteryModels()
	if len(all) != 1 {
		t.Errorf("after delete: got %d", len(all))
	}
}

func TestHistoryRecordAndLoad(t *testing.T) {
	s := freshStore(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 10; i++ {
		err := s.RecordHistory(HistoryPoint{
			TsMs:   now + int64(i)*1000,
			GridW:  float64(100 + i*10),
			PVW:    -100,
			BatW:   float64(i * 20),
			LoadW:  500,
			BatSoC: 0.5,
			JSON:   `{"i":` + "0" + `}`,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	pts, err := s.LoadHistory(now, now+10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 10 {
		t.Errorf("expected 10 points, got %d", len(pts))
	}
	if pts[0].TsMs >= pts[1].TsMs {
		t.Errorf("points should be ascending: %d vs %d", pts[0].TsMs, pts[1].TsMs)
	}
}

func TestHistoryDownsampling(t *testing.T) {
	s := freshStore(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 100; i++ {
		s.RecordHistory(HistoryPoint{
			TsMs:  now + int64(i),
			GridW: float64(i),
			JSON:  "{}",
		})
	}
	pts, err := s.LoadHistory(now, now+200, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 10 {
		t.Errorf("expected 10 downsampled points, got %d", len(pts))
	}
	if pts[0].TsMs != now || pts[len(pts)-1].TsMs != now+99 {
		t.Errorf("downsampled history endpoints = %d..%d, want %d..%d",
			pts[0].TsMs, pts[len(pts)-1].TsMs, now, now+99)
	}
}

func TestHistoryCounts(t *testing.T) {
	s := freshStore(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		s.RecordHistory(HistoryPoint{TsMs: now + int64(i), JSON: "{}"})
	}
	hot, warm, cold, err := s.HistoryCounts()
	if err != nil {
		t.Fatal(err)
	}
	if hot != 5 || warm != 0 || cold != 0 {
		t.Errorf("counts: hot=%d warm=%d cold=%d (want 5/0/0)", hot, warm, cold)
	}
}

func TestHistoryPruneAggregates(t *testing.T) {
	s := freshStore(t)
	// Insert 20 rows, all older than HotRetention
	oldMs := time.Now().UnixMilli() - int64(HotRetention.Milliseconds()) - 24*3600*1000
	for i := 0; i < 20; i++ {
		s.RecordHistory(HistoryPoint{
			TsMs:  oldMs + int64(i)*1000,
			GridW: float64(100 + i),
			JSON:  "{}",
		})
	}
	if err := s.Prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	hot, warm, _, _ := s.HistoryCounts()
	if hot != 0 {
		t.Errorf("prune: expected hot=0 after pruning old rows, got %d", hot)
	}
	if warm == 0 {
		t.Errorf("prune: expected warm>0 (hot→warm aggregation), got 0")
	}
	t.Logf("after prune: hot=%d warm=%d", hot, warm)
}

func TestTelemetrySaveLoad(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveTelemetry("ferroamp:battery", `{"w":1500}`); err != nil {
		t.Fatal(err)
	}
	v, ok := s.LoadTelemetry("ferroamp:battery")
	if !ok || v != `{"w":1500}` {
		t.Errorf("telemetry: got %q ok=%v", v, ok)
	}
}

func TestHistoryMultiTierMerge(t *testing.T) {
	s := freshStore(t)
	// Insert manually into each tier with overlapping timestamps
	now := time.Now().UnixMilli()
	if _, err := s.db.Exec(`INSERT INTO history_hot (ts_ms, json) VALUES (?, ?)`, now+1000, `{"t":"hot"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO history_warm (ts_ms, json) VALUES (?, ?)`, now+1000, `{"t":"warm"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO history_cold (ts_ms, json) VALUES (?, ?)`, now+2000, `{"t":"cold"}`); err != nil {
		t.Fatal(err)
	}
	pts, err := s.LoadHistory(now, now+10000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Errorf("expected 2 unique timestamps after dedup, got %d: %+v", len(pts), pts)
	}
	// 1000 should be hot (tier 0 wins over warm tier 1)
	for _, p := range pts {
		if p.TsMs == now+1000 && p.JSON != `{"t":"hot"}` {
			t.Errorf("dedup should prefer hot at ts=%d: got %s", p.TsMs, p.JSON)
		}
	}
}

func TestSnapshotToCapturesLiveState(t *testing.T) {
	s := freshStore(t)
	// Seed content that must survive the snapshot — value rows across
	// a couple of tables so we verify VACUUM INTO copied more than an
	// empty schema.
	if err := s.SaveConfig("mode", "planner_self"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveTelemetry("ferroamp:battery", `{"w":1500,"soc":0.42}`); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(dst); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}

	// Source DB still works after snapshot (no locks / corruption).
	if err := s.SaveConfig("post_snap", "ok"); err != nil {
		t.Errorf("source DB unusable after snapshot: %v", err)
	}

	// Snapshot DB opens cleanly and contains the seeded rows.
	snap, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	t.Cleanup(func() { snap.Close() })

	if v, ok := snap.LoadConfig("mode"); !ok || v != "planner_self" {
		t.Errorf("snapshot missing config row: got %q ok=%v", v, ok)
	}
	if v, ok := snap.LoadTelemetry("ferroamp:battery"); !ok || v != `{"w":1500,"soc":0.42}` {
		t.Errorf("snapshot missing telemetry row: got %q ok=%v", v, ok)
	}
	// Snapshot was taken BEFORE post_snap was saved — it must NOT be
	// present, proving the snapshot is point-in-time.
	if _, ok := snap.LoadConfig("post_snap"); ok {
		t.Error("snapshot contains row written after snapshot — VACUUM INTO isn't point-in-time as assumed")
	}
}

func TestSnapshotToPreservesTrustedDevicePubkeys(t *testing.T) {
	s := freshStore(t)
	credID := []byte("credential-with-browser-keys")
	if err := s.SaveTrustedDevice(TrustedDevice{
		CredentialID:   credID,
		PublicKey:      []byte("credential-public-key"),
		FriendlyName:   "Safari",
		CreatedAtMs:    1700000000000,
		DevicePubkey:   "browser-key-at-enroll",
		WalletHandle:   "wallet",
		BackupEligible: true,
		BackupState:    true,
	}); err != nil {
		t.Fatalf("save trusted device: %v", err)
	}
	if err := s.SetTrustedDevicePubkey(credID, "browser-key-after-login", false); err != nil {
		t.Fatalf("set trusted device pubkey: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(dst); err != nil {
		t.Fatalf("SnapshotTo with trusted_device_pubkeys: %v", err)
	}

	snap, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	t.Cleanup(func() { snap.Close() })

	records, err := snap.TrustedDevicePubkeyRecords()
	if err != nil {
		t.Fatalf("snapshot trusted device pubkeys: %v", err)
	}
	got := map[string]bool{}
	for _, r := range records {
		got[r.DevicePubkey] = true
	}
	for _, want := range []string{"browser-key-at-enroll", "browser-key-after-login"} {
		if !got[want] {
			t.Fatalf("snapshot missing trusted device pubkey %q; records=%+v", want, records)
		}
	}

	rows, err := snap.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("snapshot has foreign-key violations")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
}

// 2026-05-25 performance fix: snapshots now skip the bulky time-series
// tables (history_hot/warm/cold + ts_samples) which are recoverable
// from cold parquet roll-off anyway. Verify that essential tables
// (config, telemetry, devices, prices, etc.) ARE preserved AND the
// excluded tables exist but are empty in the snapshot.
func TestSnapshotToSkipsTimeSeriesTables(t *testing.T) {
	s := freshStore(t)
	// Essential row that MUST survive the snapshot.
	if err := s.SaveConfig("mode", "passive_arbitrage"); err != nil {
		t.Fatal(err)
	}
	// Seed a history_hot row so we can verify exclusion. RecordHistory
	// writes into history_hot directly.
	if err := s.RecordHistory(HistoryPoint{
		TsMs:  time.Now().UnixMilli(),
		GridW: 1234, PVW: -2345, BatW: 567, LoadW: 890,
	}); err != nil {
		t.Fatal(err)
	}
	// Seed a long-format TS sample so ts_samples has rows too.
	if err := s.RecordSamples([]Sample{
		{Driver: "ferroamp", Metric: "pv_w", TsMs: time.Now().UnixMilli(), Value: -3000},
	}); err != nil {
		t.Fatal(err)
	}

	// Sanity: source DB has the rows we just wrote.
	if hot, _, _, err := s.HistoryCounts(); err != nil || hot == 0 {
		t.Fatalf("seed: history_hot rows = %d (err=%v) — want > 0", hot, err)
	}

	dst := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(dst); err != nil {
		t.Fatalf("SnapshotTo: %v", err)
	}
	snap, err := Open(dst)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	t.Cleanup(func() { snap.Close() })

	// Essentials preserved.
	if v, ok := snap.LoadConfig("mode"); !ok || v != "passive_arbitrage" {
		t.Errorf("snapshot dropped config row: got %q ok=%v", v, ok)
	}
	// Time-series excluded — tables exist (Open runs migrate()) but
	// rows must NOT be present.
	if hot, warm, cold, err := snap.HistoryCounts(); err != nil {
		t.Errorf("HistoryCounts on snap: %v", err)
	} else if hot+warm+cold != 0 {
		t.Errorf("snapshot history rows = %d+%d+%d — want 0 (excluded)", hot, warm, cold)
	}
	// ts_samples: query directly since there's no public counter.
	var nSamples int
	if err := snap.db.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&nSamples); err != nil {
		t.Errorf("count ts_samples: %v", err)
	} else if nSamples != 0 {
		t.Errorf("snapshot ts_samples rows = %d — want 0 (excluded)", nSamples)
	}
}

func TestSnapshotToRefusesExistingFile(t *testing.T) {
	s := freshStore(t)
	dst := filepath.Join(t.TempDir(), "snap.db")
	// First snapshot succeeds.
	if err := s.SnapshotTo(dst); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	// Second snapshot to the SAME path must fail — SQLite refuses to
	// overwrite. This is intentional so a caller can't accidentally
	// stomp on a prior snapshot by reusing a timestamp or pathbuilder
	// bug.
	if err := s.SnapshotTo(dst); err == nil {
		t.Error("second SnapshotTo to existing path should fail")
	}
}

// avoid unused import if context not used
var _ = context.Canceled
