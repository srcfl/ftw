package state

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func energyPtr(v float64) *float64 { return &v }

func recordEnergyTestTick(t *testing.T, s *Store, atMS int64, observations ...EnergyObservation) {
	t.Helper()
	if err := s.RecordTickWithEnergy(HistoryPoint{TsMs: atMS, JSON: "{}"}, nil, observations); err != nil {
		t.Fatalf("RecordTickWithEnergy: %v", err)
	}
}

func ledgerObservation(assetID string, kind EnergyAssetKind, flow EnergyFlow, atMS int64, counter, power *float64) EnergyObservation {
	return EnergyObservation{
		AssetID: assetID, DeviceID: "maker:serial", AssetKind: kind, Label: "device",
		Flow: flow, AtMs: atMS, CounterWh: counter, PowerW: power,
	}
}

func loadLedgerTestPoints(t *testing.T, s *Store, assetID string, sinceMS, untilMS int64) []EnergyLedgerPoint {
	t.Helper()
	points, _, err := s.LoadEnergyHistory(EnergyHistoryQuery{
		AssetID: assetID, SinceMS: sinceMS, UntilMS: untilMS,
		BucketMS: EnergyLedgerBucketMS, Limit: 100,
	})
	if err != nil {
		t.Fatalf("LoadEnergyHistory: %v", err)
	}
	return points
}

func TestEnergyLedgerKeepsSimultaneousMeterDirections(t *testing.T) {
	s := freshStore(t)
	base := int64(1_800_000_000_000 / EnergyLedgerBucketMS * EnergyLedgerBucketMS)
	assetID := HardwareEnergyAssetID("maker:serial", AssetGridMeter)
	recordEnergyTestTick(t, s, base,
		ledgerObservation(assetID, AssetGridMeter, FlowGridImport, base, energyPtr(1000), energyPtr(500)),
		ledgerObservation(assetID, AssetGridMeter, FlowGridExport, base, energyPtr(200), energyPtr(0)),
	)
	recordEnergyTestTick(t, s, base+60_000,
		ledgerObservation(assetID, AssetGridMeter, FlowGridImport, base+60_000, energyPtr(1010), energyPtr(500)),
		ledgerObservation(assetID, AssetGridMeter, FlowGridExport, base+60_000, energyPtr(205), energyPtr(0)),
	)

	points := loadLedgerTestPoints(t, s, assetID, base, base+EnergyLedgerBucketMS)
	totals := map[EnergyFlow]float64{}
	for _, p := range points {
		if p.Quality == "measured" {
			totals[p.Flow] += p.EnergyWh
		}
	}
	if totals[FlowGridImport] != 10 || totals[FlowGridExport] != 5 {
		t.Fatalf("directional totals = %#v, want import=10 export=5", totals)
	}
}

func TestEnergyLedgerMarksCounterResetAndUsesPowerFallback(t *testing.T) {
	s := freshStore(t)
	base := int64(1_800_000_000_000 / EnergyLedgerBucketMS * EnergyLedgerBucketMS)
	assetID := HardwareEnergyAssetID("maker:serial", AssetBattery)
	recordEnergyTestTick(t, s, base,
		ledgerObservation(assetID, AssetBattery, FlowBatteryCharge, base, energyPtr(100), energyPtr(1000)))
	recordEnergyTestTick(t, s, base+60_000,
		ledgerObservation(assetID, AssetBattery, FlowBatteryCharge, base+60_000, energyPtr(110), energyPtr(1000)))
	recordEnergyTestTick(t, s, base+120_000,
		ledgerObservation(assetID, AssetBattery, FlowBatteryCharge, base+120_000, energyPtr(5), energyPtr(1000)))

	points := loadLedgerTestPoints(t, s, assetID, base, base+EnergyLedgerBucketMS)
	var sawReset, sawFallback bool
	var fallbackWh float64
	for _, p := range points {
		if p.Quality == "reset" && p.Provenance == "counter_reset" {
			sawReset = true
		}
		if p.Quality == "integrated" && p.Provenance == "counter_reset_fallback" {
			sawFallback = true
			fallbackWh += p.EnergyWh
		}
	}
	if !sawReset || !sawFallback {
		t.Fatalf("points missing reset/fallback: %+v", points)
	}
	if math.Abs(fallbackWh-1000.0/60.0) > 1e-9 {
		t.Fatalf("fallback energy = %v Wh, want %v", fallbackWh, 1000.0/60.0)
	}
}

func TestEnergyLedgerMarksPowerGapAndRecoversCounterDelta(t *testing.T) {
	s := freshStore(t)
	base := int64(1_800_000_000_000 / EnergyLedgerBucketMS * EnergyLedgerBucketMS)
	powerAsset := HardwareEnergyAssetID("maker:power", AssetObservedConsumer)
	counterAsset := HardwareEnergyAssetID("maker:counter", AssetGridMeter)
	recordEnergyTestTick(t, s, base,
		ledgerObservation(powerAsset, AssetObservedConsumer, FlowConsumerUse, base, nil, energyPtr(900)),
		ledgerObservation(counterAsset, AssetGridMeter, FlowGridImport, base, energyPtr(100), energyPtr(900)),
	)
	recordEnergyTestTick(t, s, base+180_000,
		ledgerObservation(powerAsset, AssetObservedConsumer, FlowConsumerUse, base+180_000, nil, energyPtr(900)),
		ledgerObservation(counterAsset, AssetGridMeter, FlowGridImport, base+180_000, energyPtr(130), energyPtr(900)),
	)

	powerPoints := loadLedgerTestPoints(t, s, powerAsset, base, base+EnergyLedgerBucketMS)
	if !hasLedgerQuality(powerPoints, "gap", "power_gap") {
		t.Fatalf("power gap not marked: %+v", powerPoints)
	}
	counterPoints := loadLedgerTestPoints(t, s, counterAsset, base, base+EnergyLedgerBucketMS)
	if !hasLedgerQuality(counterPoints, "recovered", "counter_gap") {
		t.Fatalf("counter recovery not marked: %+v", counterPoints)
	}
	var recovered float64
	for _, p := range counterPoints {
		if p.Quality == "recovered" && p.Provenance == "counter_gap" {
			recovered += p.EnergyWh
		}
	}
	if recovered != 30 {
		t.Fatalf("recovered counter energy = %v, want 30", recovered)
	}
}

func TestEnergyLedgerCounterResumeAfterPowerFallbackDoesNotDoubleCount(t *testing.T) {
	s := freshStore(t)
	base := int64(1_800_000_000_000 / EnergyLedgerBucketMS * EnergyLedgerBucketMS)
	assetID := HardwareEnergyAssetID("maker:resume", AssetGridMeter)
	recordEnergyTestTick(t, s, base,
		ledgerObservation(assetID, AssetGridMeter, FlowGridImport, base, energyPtr(100), energyPtr(3600)))
	recordEnergyTestTick(t, s, base+60_000,
		ledgerObservation(assetID, AssetGridMeter, FlowGridImport, base+60_000, nil, energyPtr(3600)))
	// The returning counter's 120 Wh delta spans both fallback minutes. It is
	// re-baselined instead of applied; only the final minute is integrated.
	recordEnergyTestTick(t, s, base+120_000,
		ledgerObservation(assetID, AssetGridMeter, FlowGridImport, base+120_000, energyPtr(220), energyPtr(3600)))
	// Counter deltas are authoritative again after the resume baseline.
	recordEnergyTestTick(t, s, base+180_000,
		ledgerObservation(assetID, AssetGridMeter, FlowGridImport, base+180_000, energyPtr(280), energyPtr(3600)))

	points := loadLedgerTestPoints(t, s, assetID, base, base+EnergyLedgerBucketMS)
	var total float64
	var resumeFallback, resumed bool
	for _, p := range points {
		total += p.EnergyWh
		resumeFallback = resumeFallback || p.Provenance == "counter_resume_fallback"
		resumed = resumed || p.Provenance == "counter_resumed"
	}
	if math.Abs(total-180) > 1e-9 {
		t.Fatalf("energy after fallback + counter resume = %v Wh, want 180 without overlap", total)
	}
	if !resumeFallback || !resumed {
		t.Fatalf("resume provenance missing: %+v", points)
	}
}

func TestEnergyLedgerRollupBoundaryPreservesTotalsAndResolution(t *testing.T) {
	s := freshStore(t)
	now := time.Date(2028, 7, 20, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-EnergyLedgerDetailedRetention).UnixMilli()
	cutoff = cutoff / EnergyLedgerRollupBucketMS * EnergyLedgerRollupBucketMS
	oldHour := cutoff - EnergyLedgerRollupBucketMS
	assetID := HardwareEnergyAssetID("maker:tier", AssetGridMeter)

	for i := int64(0); i < 12; i++ {
		insertLedgerEntryTest(t, s, assetID, FlowGridImport, oldHour+i*EnergyLedgerBucketMS,
			EnergyLedgerBucketMS, 1, "hardware_counter", "measured", "counter", 1)
		insertLedgerEntryTest(t, s, assetID, FlowGridExport, oldHour+i*EnergyLedgerBucketMS,
			EnergyLedgerBucketMS, 0.5, "hardware_counter", "recovered", "counter_gap", 1)
	}
	// Exactly on the tier boundary remains five-minute detail.
	insertLedgerEntryTest(t, s, assetID, FlowGridImport, cutoff,
		EnergyLedgerBucketMS, 2, "power_telemetry", "integrated", "power", 1)
	// This source is first rolled up and then removed by hard retention.
	ancient := now.Add(-EnergyLedgerRetention - time.Hour).UnixMilli()
	insertLedgerEntryTest(t, s, assetID, FlowGridImport, ancient,
		EnergyLedgerBucketMS, 99, "hardware_counter", "measured", "counter", 1)

	rolled, expired, err := s.PruneEnergyLedger(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if rolled != 25 || expired != 1 {
		t.Fatalf("maintenance counts rolled=%d expired=%d, want 25/1", rolled, expired)
	}
	rolled, expired, err = s.PruneEnergyLedger(context.Background(), now)
	if err != nil || rolled != 0 || expired != 0 {
		t.Fatalf("second maintenance must be idempotent: rolled=%d expired=%d err=%v", rolled, expired, err)
	}

	points, truncated, err := s.LoadEnergyHistory(EnergyHistoryQuery{
		AssetID: assetID, SinceMS: oldHour, UntilMS: cutoff + EnergyLedgerRollupBucketMS,
		BucketMS: EnergyLedgerBucketMS, Limit: 100,
	})
	if err != nil || truncated {
		t.Fatalf("load boundary: truncated=%v err=%v", truncated, err)
	}
	var importWh, exportWh float64
	var sawHourlyImport, sawHourlyExport, sawDetailed bool
	for _, p := range points {
		if p.Flow == FlowGridImport {
			importWh += p.EnergyWh
		}
		if p.Flow == FlowGridExport {
			exportWh += p.EnergyWh
		}
		if p.BucketStartMS == oldHour && p.Flow == FlowGridImport {
			sawHourlyImport = p.BucketLenMS == EnergyLedgerRollupBucketMS &&
				p.Quality == "measured" && p.Provenance == "counter"
		}
		if p.BucketStartMS == oldHour && p.Flow == FlowGridExport {
			sawHourlyExport = p.BucketLenMS == EnergyLedgerRollupBucketMS &&
				p.Quality == "recovered" && p.Provenance == "counter_gap"
		}
		if p.BucketStartMS == cutoff {
			sawDetailed = p.BucketLenMS == EnergyLedgerBucketMS
		}
	}
	if importWh != 14 || exportWh != 6 || !sawHourlyImport || !sawHourlyExport || !sawDetailed {
		t.Fatalf("tier boundary lost truth: import=%v export=%v points=%+v", importWh, exportWh, points)
	}
}

func TestEnergyLedgerRollupChunkIsAtomic(t *testing.T) {
	s := freshStore(t)
	now := time.Date(2028, 7, 20, 12, 0, 0, 0, time.UTC)
	start := now.Add(-EnergyLedgerDetailedRetention - time.Hour).UnixMilli()
	start = start / EnergyLedgerBucketMS * EnergyLedgerBucketMS
	assetID := HardwareEnergyAssetID("maker:atomic", AssetGridMeter)
	insertLedgerEntryTest(t, s, assetID, FlowGridImport, start,
		EnergyLedgerBucketMS, 7, "hardware_counter", "measured", "counter", 1)

	// Abort after the hourly INSERT but before the detailed DELETE can finish.
	// Both operations must roll back together.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_energy_detail_delete
		BEFORE DELETE ON energy_ledger_entries
		WHEN OLD.bucket_len_ms = 300000
		BEGIN SELECT RAISE(ABORT, 'test rollback'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PruneEnergyLedger(context.Background(), now); err == nil {
		t.Fatal("rollup should fail when its source delete is rejected")
	}
	var detailed, hourly int
	if err := s.db.QueryRow(`SELECT
		COUNT(*) FILTER (WHERE bucket_len_ms = ?),
		COUNT(*) FILTER (WHERE bucket_len_ms = ?)
		FROM energy_ledger_entries WHERE asset_id = ?`,
		EnergyLedgerBucketMS, EnergyLedgerRollupBucketMS, assetID).Scan(&detailed, &hourly); err != nil {
		t.Fatal(err)
	}
	if detailed != 1 || hourly != 0 {
		t.Fatalf("failed rollup was partially committed: detailed=%d hourly=%d", detailed, hourly)
	}
}

func insertLedgerEntryTest(t *testing.T, s *Store, assetID string, flow EnergyFlow, startMS, lenMS int64,
	energyWh float64, source, quality, provenance string, samples int64) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO energy_ledger_entries(
		schema_version, asset_id, flow, bucket_start_ms, bucket_len_ms, energy_wh,
		source, quality, provenance, sample_count, observed_at_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, EnergyLedgerSchemaVersion, assetID,
		flow, startMS, lenMS, energyWh, source, quality, provenance, samples, startMS+lenMS)
	if err != nil {
		t.Fatal(err)
	}
}

func hasLedgerQuality(points []EnergyLedgerPoint, quality, provenance string) bool {
	for _, p := range points {
		if p.Quality == quality && p.Provenance == provenance {
			return true
		}
	}
	return false
}

func TestEnergyLedgerAssetSurvivesDriverRename(t *testing.T) {
	s := freshStore(t)
	firstID, err := s.RegisterDevice(Device{DriverName: "old-name", Make: "Maker", Serial: "ABC-42"})
	if err != nil {
		t.Fatal(err)
	}
	base := int64(1_800_000_000_000 / EnergyLedgerBucketMS * EnergyLedgerBucketMS)
	assetID := HardwareEnergyAssetID(firstID, AssetBattery)
	o := ledgerObservation(assetID, AssetBattery, FlowBatteryCharge, base, energyPtr(10), energyPtr(0))
	o.DeviceID, o.Label = firstID, "old-name"
	recordEnergyTestTick(t, s, base, o)

	secondID, err := s.RegisterDevice(Device{DriverName: "new-name", Make: "Maker", Serial: "ABC-42"})
	if err != nil {
		t.Fatal(err)
	}
	if secondID != firstID {
		t.Fatalf("device identity changed across rename: %q -> %q", firstID, secondID)
	}
	o = ledgerObservation(assetID, AssetBattery, FlowBatteryCharge, base+60_000, energyPtr(15), energyPtr(0))
	o.DeviceID, o.Label = secondID, "new-name"
	recordEnergyTestTick(t, s, base+60_000, o)

	assets, err := s.EnergyAssets()
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].AssetID != assetID || assets[0].Label != "new-name" {
		t.Fatalf("assets after rename = %+v", assets)
	}
}

func TestEnergyLedgerMigrationIsAdditiveAndPreservesHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE history_hot (
		ts_ms INTEGER PRIMARY KEY NOT NULL,
		grid_w REAL, pv_w REAL, bat_w REAL, load_w REAL, bat_soc REAL,
		json TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO history_hot(ts_ms, grid_w, json) VALUES(1234, 567, '{"legacy":true}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	rows, err := s.LoadHistory(1234, 1234, 0)
	if err != nil || len(rows) != 1 || rows[0].GridW != 567 || rows[0].JSON != `{"legacy":true}` {
		t.Fatalf("legacy history changed: rows=%+v err=%v", rows, err)
	}
	var version string
	if err := s.db.QueryRow(`SELECT value FROM energy_ledger_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatalf("ledger schema missing after migration: %v", err)
	}
	if version != "1" {
		t.Fatalf("ledger version = %q, want 1", version)
	}
}

func TestEnergyLedgerRejectsNewerSchemaWithoutChangingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE energy_ledger_meta SET value='2' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("opening a newer ledger schema should fail safely")
	}
	db, err := openRaw(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version string
	if err := db.QueryRow(`SELECT value FROM energy_ledger_meta WHERE key='schema_version'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "2" {
		t.Fatalf("newer schema was modified: %q", version)
	}
}
