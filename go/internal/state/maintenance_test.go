package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func touchDayFile(t *testing.T, root string, day time.Time) string {
	t.Helper()
	dir := filepath.Join(root, day.Format("2006/01"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, day.Format("02")+".parquet")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPruneColdParquet(t *testing.T) {
	coldDir := t.TempDir()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	oldSamples := touchDayFile(t, coldDir, now.AddDate(0, 0, -400))
	freshSamples := touchDayFile(t, coldDir, now.AddDate(0, 0, -10))
	oldDiag := touchDayFile(t, filepath.Join(coldDir, "diagnostics"), now.AddDate(0, 0, -400))
	freshDiag := touchDayFile(t, filepath.Join(coldDir, "diagnostics"), now.AddDate(0, 0, -10))

	removed, err := PruneColdParquet(coldDir, 365, now)
	if err != nil {
		t.Fatalf("PruneColdParquet: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed %d files, want 2: %v", len(removed), removed)
	}
	for _, gone := range []string{oldSamples, oldDiag} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("expired file still present: %s", gone)
		}
	}
	for _, kept := range []string{freshSamples, freshDiag} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("fresh file removed: %s", kept)
		}
	}
	// Emptied month dir of the expired file must be gone too.
	if _, err := os.Stat(filepath.Dir(oldSamples)); !os.IsNotExist(err) {
		t.Errorf("empty month dir not cleaned: %s", filepath.Dir(oldSamples))
	}

	// Retention 0 = keep everything.
	removed, err = PruneColdParquet(coldDir, 0, now)
	if err != nil || removed != nil {
		t.Fatalf("retention 0 must be a no-op, got removed=%v err=%v", removed, err)
	}
}

func TestRecordTickWritesHistoryAndSamplesAtomically(t *testing.T) {
	s := freshStore(t)
	now := time.Now().UnixMilli()
	err := s.RecordTick(
		HistoryPoint{TsMs: now, GridW: 1000, JSON: "{}"},
		[]Sample{
			{Driver: "hp", Metric: "hp_temp_c", TsMs: now, Value: 42, Unit: "°C"},
			{Driver: "hp", Metric: "hp_power_w", TsMs: now, Value: 500},
		},
	)
	if err != nil {
		t.Fatalf("RecordTick: %v", err)
	}

	hist, err := s.LoadHistory(now-1, now+1, 0)
	if err != nil || len(hist) != 1 || hist[0].GridW != 1000 {
		t.Fatalf("history point not written: %v %v", hist, err)
	}
	sm, err := s.LatestSample("hp", "hp_temp_c")
	if err != nil || sm.Value != 42 {
		t.Fatalf("sample not written: %+v %v", sm, err)
	}
	catalog, err := s.MetricsCatalog()
	if err != nil {
		t.Fatal(err)
	}
	units := map[string]string{}
	for _, m := range catalog {
		units[m.Name] = m.Unit
	}
	if units["hp_temp_c"] != "°C" {
		t.Fatalf("unit not persisted through RecordTick: %v", units)
	}

	// Empty samples must still write the history point.
	if err := s.RecordTick(HistoryPoint{TsMs: now + 1, GridW: 900, JSON: "{}"}, nil); err != nil {
		t.Fatalf("RecordTick without samples: %v", err)
	}
}

func TestCheckpointWAL(t *testing.T) {
	s := freshStore(t)
	if err := s.RecordHistory(HistoryPoint{TsMs: 1, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	// Must be callable at any time without disturbing the store.
	s.CheckpointWAL()
	if err := s.RecordHistory(HistoryPoint{TsMs: 2, JSON: "{}"}); err != nil {
		t.Fatalf("store broken after CheckpointWAL: %v", err)
	}
}
