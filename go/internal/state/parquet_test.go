package state

import (
	"context"
	"testing"
	"time"
)

// Regression: the hourly rolloff writes the same UTC day repeatedly (the
// cutoff advances one hour per run). Diagnostics day files must merge, not
// overwrite — an overwrite leaves only the newest run's rows in cold storage.
func TestRolloffDiagnosticsMergesRepeatedSameDayRuns(t *testing.T) {
	st := openTestStore(t)
	coldDir := t.TempDir()
	day := time.Now().Add(-60 * 24 * time.Hour).UTC().Truncate(24 * time.Hour)

	firstTs := day.Add(2 * time.Hour).UnixMilli()
	if err := st.SaveDiagnostic(firstTs, "scheduled", "SE3", 1, 96, `{"run":1}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RolloffDiagnosticsToParquet(context.Background(), coldDir); err != nil {
		t.Fatalf("first rolloff: %v", err)
	}

	secondTs := day.Add(3 * time.Hour).UnixMilli()
	if err := st.SaveDiagnostic(secondTs, "scheduled", "SE3", 2, 96, `{"run":2}`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RolloffDiagnosticsToParquet(context.Background(), coldDir); err != nil {
		t.Fatalf("second rolloff: %v", err)
	}

	got, err := st.LoadDiagnosticsFromParquet(coldDir, day.UnixMilli(), day.Add(24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("LoadDiagnosticsFromParquet: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("cold diagnostics len = %d, want 2 (second run overwrote the first?): %+v", len(got), got)
	}
	if got[0].TsMs != firstTs || got[1].TsMs != secondTs {
		t.Fatalf("cold diagnostics = %+v, want ts %d and %d", got, firstTs, secondTs)
	}
}

// The rolloff streams day by day (flush on UTC-day change) instead of
// buffering the whole backlog. A multi-day backlog must still produce one
// complete file per day.
func TestRolloffToParquetMultiDayBacklog(t *testing.T) {
	s := freshStore(t)
	coldDir := t.TempDir()
	base := time.Now().Add(-RecentRetention - 96*time.Hour).UTC().Truncate(24 * time.Hour)

	var samples []Sample
	perDay := 5
	days := 3
	for d := 0; d < days; d++ {
		for i := 0; i < perDay; i++ {
			samples = append(samples, Sample{
				Driver: "meter",
				Metric: "grid_w",
				TsMs:   base.AddDate(0, 0, d).Add(time.Duration(i) * time.Hour).UnixMilli(),
				Value:  float64(d*100 + i),
			})
		}
	}
	if err := s.RecordSamples(samples); err != nil {
		t.Fatalf("record samples: %v", err)
	}

	rows, files, err := s.RolloffToParquet(context.Background(), coldDir)
	if err != nil {
		t.Fatalf("RolloffToParquet: %v", err)
	}
	if rows != int64(perDay*days) {
		t.Fatalf("rolled %d rows, want %d", rows, perDay*days)
	}
	if len(files) != days {
		t.Fatalf("wrote %d day files, want %d: %v", len(files), days, files)
	}

	got, err := s.LoadSeriesFromParquet(coldDir, "meter", "grid_w",
		base.UnixMilli(), base.AddDate(0, 0, days).UnixMilli())
	if err != nil {
		t.Fatalf("LoadSeriesFromParquet: %v", err)
	}
	if len(got) != perDay*days {
		t.Fatalf("cold series len = %d, want %d", len(got), perDay*days)
	}
	// SQLite side must be empty below the cutoff.
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ts_samples`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d samples left in SQLite after rolloff, want 0", remaining)
	}
}
