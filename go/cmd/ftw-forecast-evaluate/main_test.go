package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

const testHourMS = int64(time.Hour / time.Millisecond)

func openArchive(t *testing.T) (string, *state.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.InitForecastArchive(context.Background()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	return path, store
}

func coldBand() forecasting.Band {
	return forecasting.Band{LowW: 0, HighW: 5000, Method: forecasting.BandMethodColdStart}
}

func forecastPoint(start int64, pv, load float64) forecasting.Point {
	return forecasting.Point{
		StartMS: start, EndMS: start + testHourMS, PVW: pv, LoadW: load,
		PVKnown: true, LoadKnown: true, PVQuality: "forecast", LoadQuality: "forecast",
		PVBand: coldBand(), LoadBand: coldBand(), NetBand: coldBand(),
	}
}

func forecastIssue(id string, origin int64, points []forecasting.Point) forecasting.Issue {
	return forecasting.Issue{
		Schema: forecasting.Schema, ID: id, OriginMS: origin, IssuedAtMS: origin, LatestInputMS: origin,
		ConfigVersion: "cfg", Series: []forecasting.Series{{Name: "champion", ModelVersion: "v1", Points: points}},
	}
}

func observation(start, available int64, pv, load float64) forecasting.Observation {
	return forecasting.Observation{
		StartMS: start, EndMS: start + testHourMS, AvailableAtMS: available,
		PVW: pv, LoadW: load, PVKnown: true, LoadKnown: true, Quality: "complete", ConfigVersion: "cfg",
	}
}

func runReport(t *testing.T, path string, since, until, now time.Time) report {
	t.Helper()
	var output bytes.Buffer
	args := []string{"-state", path, "-since", since.Format(time.RFC3339), "-until", until.Format(time.RFC3339)}
	if err := run(context.Background(), args, &output, now); err != nil {
		t.Fatal(err)
	}
	var got report
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestEvaluateEmptyArchive(t *testing.T) {
	path, store := openArchive(t)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	got := runReport(t, path, now.Add(-24*time.Hour), now, now)
	if got.IssueCount != 0 || got.TruthCount != 0 || len(got.PerLead) != 0 ||
		len(got.PairedMetrics) != 0 || len(got.CumulativeNetError) != 0 {
		t.Fatalf("empty archive report = %+v", got)
	}
}

func TestEvaluateExcludesTruthUnavailableAtAsOfAndRejectsGap(t *testing.T) {
	path, store := openArchive(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-6 * time.Hour)
	points := []forecasting.Point{
		forecastPoint(start.UnixMilli(), 100, 1000),
		forecastPoint(start.Add(time.Hour).UnixMilli(), 100, 1000),
		forecastPoint(start.Add(2*time.Hour).UnixMilli(), 100, 1000),
	}
	issue := forecastIssue("gap", start.Add(-2*time.Hour).UnixMilli(), points)
	if err := store.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForecastObservation(ctx, observation(start.UnixMilli(), start.Add(time.Hour).UnixMilli(), 200, 1200)); err != nil {
		t.Fatal(err)
	}
	// The middle hour is missing. The last hour exists but only becomes known
	// after this report's as-of time.
	futureReceipt := start.Add(4 * time.Hour).UnixMilli()
	if err := store.SaveForecastObservation(ctx, observation(start.Add(2*time.Hour).UnixMilli(), futureReceipt, 200, 1200)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	until := start.Add(3 * time.Hour)
	got := runReport(t, path, start.Add(-3*time.Hour), until, now)
	if got.TruthCount != 1 {
		t.Fatalf("truth count=%d, want only the causally available row", got.TruthCount)
	}
	for _, metric := range got.CumulativeNetError {
		if metric.Hours >= 3 {
			t.Fatalf("gap or unavailable truth produced a %dh metric: %+v", metric.Hours, metric)
		}
	}
}

func TestEvaluateDeduplicatesOriginsAndReportsAllEnergyHorizons(t *testing.T) {
	path, store := openArchive(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-25 * time.Hour)
	olderPoints := make([]forecasting.Point, 24)
	newerPoints := make([]forecasting.Point, 24)
	energyplanPoints := make([]forecasting.Point, 24)
	for i := 0; i < 24; i++ {
		at := start.Add(time.Duration(i) * time.Hour).UnixMilli()
		olderPoints[i] = forecastPoint(at, 100, 1000)
		newerPoints[i] = forecastPoint(at, 200, 1000)
		energyplanPoints[i] = forecastPoint(at, 250, 1200)
		if err := store.SaveForecastObservation(ctx, observation(at, at+testHourMS, 300, 1300)); err != nil {
			t.Fatal(err)
		}
	}
	older := forecastIssue("older", start.Add(-2*time.Hour-10*time.Minute).UnixMilli(), olderPoints)
	older.Series = append(older.Series, forecasting.Series{Name: "legacy_shadow", ModelVersion: "v1", Points: olderPoints})
	newer := forecastIssue("newer", start.Add(-2*time.Hour).UnixMilli(), newerPoints)
	newer.Series = append(newer.Series,
		forecasting.Series{Name: "legacy_shadow", ModelVersion: "v1", Points: olderPoints},
		forecasting.Series{Name: "energyplan", ModelVersion: "v1", Points: energyplanPoints})
	if err := store.SaveForecastIssue(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForecastIssue(ctx, older); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	until := start.Add(24 * time.Hour)
	got := runReport(t, path, start.Add(-3*time.Hour), until, now)
	if got.IssueCount != 2 || got.TruthCount != 24 {
		t.Fatalf("counts=(%d,%d), want (2,24)", got.IssueCount, got.TruthCount)
	}
	foundChampionPV := false
	for _, metric := range got.PerLead {
		if metric.Series == "champion" && metric.Signal == "pv" && metric.Samples > 0 {
			foundChampionPV = true
			if math.Abs(metric.MAEW-100) > 1e-9 {
				t.Fatalf("champion PV MAE=%v, older origin was not removed", metric.MAEW)
			}
		}
	}
	if !foundChampionPV {
		t.Fatal("missing champion PV metric")
	}
	horizons := map[int]bool{}
	for _, metric := range got.CumulativeNetError {
		if metric.Series == "champion" {
			horizons[metric.Hours] = true
			if metric.Hours == 24 && math.Abs(metric.BiasWh-4800) > 1e-9 {
				t.Fatalf("24h bias=%v, want newest-origin 4800Wh", metric.BiasWh)
			}
		}
	}
	for _, hours := range []int{1, 3, 6, 12, 24} {
		if !horizons[hours] {
			t.Fatalf("missing %dh cumulative summary: %+v", hours, got.CumulativeNetError)
		}
	}
	if len(got.PairedMetrics) == 0 {
		t.Fatal("matched primary/legacy shadow targets produced no paired metrics")
	}
	if got.PrimarySeries != "champion" || got.ReferenceSeries != "legacy_shadow" {
		t.Fatalf("unexpected comparison roles: %+v", got)
	}
	for _, metric := range got.PairedMetrics {
		if metric.Champion != "champion" || metric.Candidate != "legacy_shadow" {
			t.Fatalf("raw Rust diagnostic replaced the frozen reference: %+v", metric)
		}
		if metric.Signal == "pv" && (metric.ChampionMAEW != 100 || metric.CandidateMAEW != 200 || metric.DeltaMAEW != 100) {
			t.Fatalf("composed primary/reference scores incorrect: %+v", metric)
		}
	}
}

func TestEvaluateRejectsMoreThanRetentionAndBadRFC3339(t *testing.T) {
	var output bytes.Buffer
	now := time.Now().UTC().Truncate(time.Second)
	if err := run(context.Background(), []string{"-since", "bad"}, &output, now); err == nil {
		t.Fatal("bad RFC3339 time was accepted")
	}
	if err := run(context.Background(), []string{
		"-since", now.Add(-31 * 24 * time.Hour).Format(time.RFC3339), "-until", now.Format(time.RFC3339),
	}, &output, now); err == nil {
		t.Fatal("window beyond retention was accepted")
	}
}

func TestEvaluateDoesNotPairLaterPrimaryWithOlderShadow(t *testing.T) {
	path, store := openArchive(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-2 * time.Hour)
	point := forecastPoint(start.UnixMilli(), 0, 600)
	point.PVSource, point.LoadSource = "energyplan", "legacy"
	point.PVQuality, point.LoadQuality = "cold_start", "cold_start"
	older := forecastIssue("older", start.Add(-2*time.Hour).UnixMilli(), []forecasting.Point{point})
	older.Series = append(older.Series, forecasting.Series{Name: "legacy_shadow", ModelVersion: "v1", Points: []forecasting.Point{point}})
	newer := forecastIssue("newer", start.Add(-time.Hour).UnixMilli(), []forecasting.Point{point})
	for _, issue := range []forecasting.Issue{older, newer} {
		if err := store.SaveForecastIssue(ctx, issue); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveForecastObservation(ctx, observation(start.UnixMilli(), start.Add(time.Hour).UnixMilli(), 0, 800)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	got := runReport(t, path, start.Add(-3*time.Hour), now, now)
	if len(got.PerLead) == 0 || len(got.PairedMetrics) != 0 {
		t.Fatalf("latest primary must retain standalone scores without stale pair: %+v", got)
	}
}

func TestEvaluateCanSelectHistoricalReferenceExplicitly(t *testing.T) {
	path, store := openArchive(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour)
	start := now.Add(-2 * time.Hour)
	point := forecastPoint(start.UnixMilli(), 100, 600)
	issue := forecastIssue("historical", start.Add(-time.Hour).UnixMilli(), []forecasting.Point{point})
	issue.Series = append(issue.Series, forecasting.Series{Name: "energyplan", ModelVersion: "v1", Points: []forecasting.Point{point}})
	if err := store.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForecastObservation(ctx, observation(start.UnixMilli(), start.Add(time.Hour).UnixMilli(), 0, 800)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	got := runReport(t, path, start.Add(-2*time.Hour), now, now)
	if len(got.PairedMetrics) != 0 {
		t.Fatalf("historical raw Rust silently treated as legacy reference: %+v", got)
	}
	var output bytes.Buffer
	if err := run(ctx, []string{"-state", path, "-reference", "energyplan"}, &output, now); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ReferenceSeries != "energyplan" || len(got.PairedMetrics) != 3 {
		t.Fatalf("explicit historical reference ignored: %+v", got)
	}
}

func TestBandReportExcludesRemainingInterval(t *testing.T) {
	start := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	point := forecastPoint(start, 100, 600)
	sample := forecasting.ErrorSample{Series: "champion", ConfigVersion: "cfg", IssueID: "i",
		OriginMS: start - testHourMS, IssuedAtMS: start - testHourMS, StartMS: start, EndMS: start + testHourMS,
		AvailableAtMS: start + testHourMS, Lead: 1, PVKnown: true, LoadKnown: true, Prediction: point}
	if got := summarizeBands([]forecasting.ErrorSample{sample}); len(got) != 3 {
		t.Fatalf("whole interval lost from band report: %+v", got)
	}
	sample.Prediction.PredictionStartMS = start + 60000
	if got := summarizeBands([]forecasting.ErrorSample{sample}); len(got) != 0 {
		t.Fatalf("partial interval received band coverage credit: %+v", got)
	}
}
