package state

import (
	"testing"
	"time"
)

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
