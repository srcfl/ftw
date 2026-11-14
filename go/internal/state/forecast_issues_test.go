package state

import (
	"bytes"
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
)

func openForecastArchive(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func archiveBand() forecasting.Band {
	return forecasting.Band{LowW: 0, HighW: 5000, Method: forecasting.BandMethodColdStart}
}

func archivePoint(start int64) forecasting.Point {
	return forecasting.Point{
		StartMS: start, EndMS: start + int64(time.Hour/time.Millisecond), PVW: 100, LoadW: 1000,
		PVKnown: true, LoadKnown: true, PVQuality: "forecast", LoadQuality: "forecast",
		PVBand: archiveBand(), LoadBand: archiveBand(), NetBand: archiveBand(),
	}
}

func archiveIssue(id string, origin, start int64) forecasting.Issue {
	return forecasting.Issue{
		Schema: forecasting.Schema, ID: id, OriginMS: origin, IssuedAtMS: origin, LatestInputMS: origin,
		ConfigVersion: "cfg", Series: []forecasting.Series{{Name: "champion", ModelVersion: "v1", Points: []forecasting.Point{archivePoint(start)}}},
	}
}

func archiveError(issueID string, origin, issued, start int64) forecasting.ErrorSample {
	return forecasting.ErrorSample{
		Series: "champion", ConfigVersion: "cfg", IssueID: issueID, OriginMS: origin, IssuedAtMS: issued,
		StartMS: start, EndMS: start + int64(time.Hour/time.Millisecond), AvailableAtMS: start + int64(time.Hour/time.Millisecond),
		Lead: forecasting.LeadBucket(origin, start), PVErrorW: 10, LoadErrorW: 20, PVKnown: true, LoadKnown: true,
		Prediction: archivePoint(start),
	}
}

func TestForecastIssueExternalizesAndVerifiesFullModelState(t *testing.T) {
	store := openForecastArchive(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli()
	issue := archiveIssue("one", now-2*int64(time.Hour/time.Millisecond), now)
	state := append([]byte(`{"weights":"`), bytes.Repeat([]byte("x"), 900000)...)
	state = append(state, []byte(`"}`)...)
	issue.Models = []forecasting.ModelState{{
		Name: "pv", Version: "v1", Quality: forecasting.ModelQualityWarm,
		UpdatedAtMS: issue.OriginMS, State: state,
	}}
	if err := store.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatal(err)
	}
	if issue.Models[0].StateID != "" || !bytes.Equal(issue.Models[0].State, state) {
		t.Fatal("saving an issue mutated the caller's model snapshot")
	}
	if err := store.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatalf("identical retry with inline state failed: %v", err)
	}
	issues, err := store.LoadForecastIssues(ctx, 0, time.Now().Add(time.Hour).UnixMilli(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || len(issues[0].Models) != 1 || issues[0].Models[0].StateID == "" || len(issues[0].Models[0].State) != 0 {
		t.Fatalf("stored issue did not contain one light model reference: %+v", issues)
	}
	loaded, err := store.LoadForecastModelState(ctx, issues[0].Models[0].StateID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, state) {
		t.Fatal("hydrated model state differs from issued state")
	}
	if err = store.SaveForecastIssue(ctx, issues[0]); err != nil {
		t.Fatalf("retry with an existing state reference failed: %v", err)
	}
	second := issue
	second.ID = "two"
	if err = store.SaveForecastIssue(ctx, second); err != nil {
		t.Fatal(err)
	}
	var blobs int
	if err = store.db.QueryRow("SELECT COUNT(*) FROM forecast_model_states").Scan(&blobs); err != nil || blobs != 1 {
		t.Fatalf("content-addressed states=%d err=%v, want 1", blobs, err)
	}
}

func TestForecastIssueStoresEmptyColdStartModel(t *testing.T) {
	store := openForecastArchive(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli()
	issue := archiveIssue("cold", start-2*int64(time.Hour/time.Millisecond), start)
	issue.Models = []forecasting.ModelState{{
		Name: "load", Version: "v1", Quality: forecasting.ModelQualityColdStart,
	}}
	if err := store.SaveForecastIssue(ctx, issue); err != nil {
		t.Fatalf("save empty cold-start model: %v", err)
	}
	issues, err := store.LoadForecastIssues(ctx, 0, time.Now().Add(time.Hour).UnixMilli(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || len(issues[0].Models) != 1 || issues[0].Models[0].Quality != forecasting.ModelQualityColdStart ||
		issues[0].Models[0].StateID != "" || len(issues[0].Models[0].State) != 0 {
		t.Fatalf("cold-start model changed in archive: %+v", issues)
	}
}

func TestForecastIssueImmutabilityAndSliceLimit(t *testing.T) {
	store := openForecastArchive(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour).UnixMilli()
	origin := start - 2*int64(time.Hour/time.Millisecond)
	one := archiveIssue("one", origin, start)
	two := archiveIssue("two", origin+1, start)
	if err := store.SaveForecastIssue(ctx, one); err != nil {
		t.Fatal(err)
	}
	changed := one
	changed.Series[0].Points[0].PVW++
	if err := store.SaveForecastIssue(ctx, changed); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("changed retry error=%v, want immutable", err)
	}
	if err := store.SaveForecastIssue(ctx, two); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadForecastIssues(ctx, 0, time.Now().Add(time.Hour).UnixMilli(), 1); err == nil || !strings.Contains(err.Error(), "slice limit") {
		t.Fatalf("truncated slice load error=%v", err)
	}
}

func TestForecastObservationPrunesOnEveryWrite(t *testing.T) {
	store := openForecastArchive(t)
	ctx := context.Background()
	hour := int64(time.Hour / time.Millisecond)
	oldStart := time.Now().Add(-ForecastIssueRetention - 2*time.Hour).Truncate(time.Hour).UnixMilli()
	old := forecasting.Observation{
		StartMS: oldStart, EndMS: oldStart + hour, AvailableAtMS: oldStart + hour,
		PVW: 10, LoadW: 100, PVKnown: true, LoadKnown: true, Quality: "complete", ConfigVersion: "cfg",
	}
	if err := store.SaveForecastObservation(ctx, old); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM forecast_observations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired observations=%d err=%v, want 0", count, err)
	}
}

func TestForecastErrorsValidateAndUseFullTieBreak(t *testing.T) {
	store := openForecastArchive(t)
	ctx := context.Background()
	hour := int64(time.Hour / time.Millisecond)
	start := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour).UnixMilli()
	origin := start - 2*hour
	a := archiveError("a", origin, origin, start)
	z := archiveError("z", origin, origin, start)
	z.PVErrorW = 30
	if err := store.SaveForecastErrors(ctx, []forecasting.ErrorSample{a}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveForecastErrors(ctx, []forecasting.ErrorSample{z}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadForecastErrors(ctx, start-hour, time.Now().UnixMilli(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].IssueID != "z" || loaded[0].PVErrorW != 30 {
		t.Fatalf("stored tie winner=%+v, want issue z", loaded)
	}
	bad := z
	bad.StartMS += hour
	bad.EndMS += hour
	bad.AvailableAtMS += hour
	bad.Prediction.StartMS = bad.StartMS
	bad.Prediction.EndMS = bad.EndMS
	bad.Lead = forecasting.LeadBucket(bad.OriginMS, bad.StartMS)
	bad.PVErrorW = math.NaN()
	if err = store.SaveForecastErrors(ctx, []forecasting.ErrorSample{bad}, time.Now().Add(time.Hour).UnixMilli()); err == nil {
		t.Fatal("nonfinite residual was stored")
	}
}

func TestForecastHourlyLoadAfterIndexUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	const hour int64 = 3600000
	const start = 30000 * hour
	const until = start + 3*hour
	var indexes int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='forecast_errors_hourly'").Scan(&indexes); err != nil || indexes != 0 {
		t.Fatalf("archive initialization created %d hourly indexes, err=%v", indexes, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.EnsureForecastHourlyIndex(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled index creation returned %v", err)
	}
	samples := []forecasting.ErrorSample{
		archiveError("hourly", start-hour, start-hour, start),
		archiveError("tied", start-hour, start-hour, start),
		archiveError("quarter", start-hour, start-hour, start+hour/4),
		archiveError("next", start, start, start+hour),
		archiveError("late", start, start, start+2*hour),
	}
	samples[1].ConfigVersion = "other"
	samples[4].AvailableAtMS = until + 1
	if err := s.SaveForecastErrors(ctx, samples, until+1); err != nil {
		t.Fatal(err)
	}
	before, err := s.LoadForecastErrors(ctx, start, until, true)
	if err != nil || len(before) != 3 {
		t.Fatalf("pre-upgrade hourly load returned %d rows, err=%v", len(before), err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := OpenWithBackgroundHistory(path, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s = upgraded
	after, err := s.LoadForecastErrors(ctx, start, until, true)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("hourly history changed after upgrade: before=%v after=%v err=%v", before, after, err)
	}
	all, err := s.LoadForecastErrors(ctx, start, until, false)
	if err != nil || len(all) != 4 {
		t.Fatalf("all-slot load returned %d rows, err=%v", len(all), err)
	}

	rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+forecastErrorLoadQuery(true), start, until, until, MaxForecastErrors+1)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var usesHourlyIndex bool
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "TEMP B-TREE") {
			t.Fatalf("unexpected sort: %s", detail)
		}
		usesHourlyIndex = usesHourlyIndex || strings.Contains(detail, "USING INDEX forecast_errors_hourly")
	}
	if err := rows.Err(); err != nil || !usesHourlyIndex {
		t.Fatalf("hourly index selected=%t err=%v", usesHourlyIndex, err)
	}

	if _, err := s.db.Exec("UPDATE forecast_errors SET payload='{}' WHERE issue_id='hourly'"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadForecastErrors(ctx, start, until, true); got != nil || err == nil || !strings.Contains(err.Error(), "invalid archived forecast error") {
		t.Fatalf("invalid sample returned %v, err=%v", got, err)
	}
}
