package forecasting

import (
	"testing"
	"time"
)

func TestRemainingIntervalNeverEarnsHeldOutOrCumulativeScore(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	points := make([]Point, 24)
	truth := make([]Observation, len(points))
	for i := range points {
		at := start + int64(i)*hourMS
		points[i] = testPoint(at, at+hourMS, 100, 1000)
		truth[i] = testObservation("cfg", at, at+hourMS, 100, 1000)
	}
	// Deliberately old origin/receipt: the explicit partial marker must protect
	// held-out scores even when the ordinary timestamp guard would accept it.
	points[0].PredictionStartMS = start + 10*60*1000
	issue := testIssue("remaining", "cfg", "champion", start-hourMS, start-hourMS, points)
	for _, name := range []string{"planning", "legacy_shadow", "energyplan", "last_day", "last_week", "persistence", "generic"} {
		issue.Series = append(issue.Series, Series{Name: name, ModelVersion: "v1", Points: append([]Point(nil), points...)})
	}
	if err := issue.Validate(); err != nil {
		t.Fatal(err)
	}
	errors := Errors([]Issue{issue}, truth, start+24*hourMS)
	if len(errors) != 23*MaxSeries {
		t.Fatalf("full future intervals lost or partial included: %d", len(errors))
	}
	for _, e := range errors {
		if e.StartMS == start {
			t.Fatalf("partial received whole-hour score: %+v", e)
		}
	}
	for _, m := range CompareFrozenSeries(errors, "champion", "legacy_shadow") {
		if m.Samples == 0 {
			t.Fatal("future whole intervals should still compare")
		}
	}
	cumulative := CumulativeNetEnergy([]Issue{issue}, truth, start+24*hourMS)
	if len(cumulative) == 0 {
		t.Fatal("remaining whole-hour windows must still score")
	}
	for _, sample := range cumulative {
		if sample.StartMS == start || sample.Hours == 24 {
			t.Fatalf("cumulative window included partial forecast: %+v", sample)
		}
	}
}

func TestImportedPartialErrorsCannotCalibrateOrCompare(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	var full, partial []ErrorSample
	for day := 0; day < 7; day++ {
		for hour := 0; hour < 8; hour++ {
			start := base + int64(day*24+hour)*hourMS
			e := testError("champion", "cfg", "capture", float64(start), float64(start-2*hourMS), 100, 100)
			full = append(full, e)
			e.Prediction.PredictionStartMS = start + 60000
			partial = append(partial, e)
			shadow := e
			shadow.Series = "legacy_shadow"
			partial = append(partial, shadow)
		}
	}
	origin := base + 8*24*hourMS
	if b := NewCalibrator(full, "cfg", origin).Band("champion", "pv", origin+2*hourMS, 500); b.Method != BandMethodEmpirical {
		t.Fatalf("fixture lacks full calibration coverage: %+v", b)
	}
	if err := partial[0].Validate(); err == nil {
		t.Fatal("partial error was accepted for archive/calibration")
	}
	if b := NewCalibrator(partial, "cfg", origin).Band("champion", "pv", origin+2*hourMS, 500); b.Method != BandMethodColdStart || b.Samples != 0 {
		t.Fatalf("imported partial errors earned empirical confidence: %+v", b)
	}
	if len(Metrics(partial)) != 0 || len(CompareSeries(partial, "champion", "legacy_shadow")) != 0 || len(CompareFrozenSeries(partial, "champion", "legacy_shadow")) != 0 {
		t.Fatal("imported partial errors produced held-out metrics")
	}
}
