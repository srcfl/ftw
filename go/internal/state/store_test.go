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

func TestConfigPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(path)
	if err != nil { t.Fatal(err) }
	s1.SaveConfig("greeting", "hello")
	s1.Close()

	s2, err := Open(path)
	if err != nil { t.Fatal(err) }
	defer s2.Close()
	v, ok := s2.LoadConfig("greeting")
	if !ok || v != "hello" {
		t.Errorf("persistence: got %q ok=%v", v, ok)
	}
}

func TestEventsRecorded(t *testing.T) {
	s := freshStore(t)
	for i := 0; i < 5; i++ {
		if err := s.RecordEvent("evt"); err != nil { t.Fatal(err) }
	}
	events, err := s.RecentEvents(10)
	if err != nil { t.Fatal(err) }
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

func TestBatteryModelStore(t *testing.T) {
	s := freshStore(t)
	if err := s.SaveBatteryModel("ferroamp", `{"a":0.7,"b":0.3}`); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveBatteryModel("sungrow", `{"a":0.5,"b":0.4}`); err != nil {
		t.Fatal(err)
	}
	all, err := s.LoadAllBatteryModels()
	if err != nil { t.Fatal(err) }
	if len(all) != 2 {
		t.Errorf("expected 2 models, got %d", len(all))
	}
	if all["ferroamp"] != `{"a":0.7,"b":0.3}` {
		t.Errorf("ferroamp: %s", all["ferroamp"])
	}
	if err := s.DeleteBatteryModel("sungrow"); err != nil { t.Fatal(err) }
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
		if err != nil { t.Fatal(err) }
	}
	pts, err := s.LoadHistory(now, now+10000, 0)
	if err != nil { t.Fatal(err) }
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
			TsMs: now + int64(i),
			GridW: float64(i),
			JSON: "{}",
		})
	}
	pts, err := s.LoadHistory(now, now+200, 10)
	if err != nil { t.Fatal(err) }
	if len(pts) != 10 {
		t.Errorf("expected 10 downsampled points, got %d", len(pts))
	}
}

func TestHistoryCounts(t *testing.T) {
	s := freshStore(t)
	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		s.RecordHistory(HistoryPoint{TsMs: now + int64(i), JSON: "{}"})
	}
	hot, warm, cold, err := s.HistoryCounts()
	if err != nil { t.Fatal(err) }
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
			TsMs: oldMs + int64(i)*1000,
			GridW: float64(100 + i),
			JSON: "{}",
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
	if err != nil { t.Fatal(err) }
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
