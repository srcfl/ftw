package state

import (
	"testing"
	"time"
)

func TestLiveDayEnergyUsesSQLiteOnly(t *testing.T) {
	s := freshStore(t)
	base := time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC)
	if err := s.RecordHistory(HistoryPoint{TsMs: base.UnixMilli(), GridW: 1000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(5 * time.Minute).UnixMilli(), GridW: 1000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.LiveDayEnergy(base.Add(-12*time.Hour).UnixMilli(), base.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("status energy touched DuckDB: %v", err)
	}
	if d.Intervals != 1 {
		t.Fatalf("intervals=%d", d.Intervals)
	}
}

func TestDailyEnergyUsesLiveSQLite(t *testing.T) {
	s := freshStore(t)
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	if err := s.RecordHistory(HistoryPoint{TsMs: base.UnixMilli(), GridW: 2000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHistory(HistoryPoint{TsMs: base.Add(5 * time.Minute).UnixMilli(), GridW: 2000, JSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	d, err := s.DailyEnergy(base.UnixMilli(), base.Add(time.Hour).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if d.Intervals != 1 {
		t.Fatalf("intervals=%d", d.Intervals)
	}
	want := 2000.0 * 5 / 60
	if d.ImportWh < want*0.99 || d.ImportWh > want*1.01 {
		t.Fatalf("ImportWh=%v want ~%v", d.ImportWh, want)
	}
}
