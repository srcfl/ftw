package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestForecastScoringRecoversObservationsOlderThanTwoHours(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	for day := 1; day <= 3; day++ {
		start := now.Add(-time.Duration(day) * 24 * time.Hour)
		end := start.Add(time.Hour)
		band := forecasting.Band{LowW: 0, HighW: 5000, Method: forecasting.BandMethodColdStart}
		point := forecasting.Point{StartMS: start.UnixMilli(), EndMS: end.UnixMilli(), PVW: 1000, LoadW: 500, PVKnown: true, LoadKnown: true, PVQuality: "forecast", LoadQuality: "forecast", PVBand: band, LoadBand: band, NetBand: band}
		for i := 0; i < 7; i++ {
			origin := start.Add(-2 * time.Hour).Add(time.Duration(i) * time.Second).UnixMilli()
			issue := forecasting.Issue{Schema: forecasting.Schema, ID: fmt.Sprintf("day-%d-%d", day, i), OriginMS: origin, IssuedAtMS: origin, LatestInputMS: origin, ConfigVersion: "site-v1", Series: []forecasting.Series{{Name: "champion", ModelVersion: "v1", Points: []forecasting.Point{point}}}}
			if err := store.SaveForecastIssue(ctx, issue); err != nil {
				t.Fatal(err)
			}
		}
		observation := forecasting.Observation{StartMS: start.UnixMilli(), EndMS: end.UnixMilli(), AvailableAtMS: end.UnixMilli(), ConfigVersion: "site-v1", Quality: "complete", PVW: 1300, LoadW: 600, PVKnown: true, LoadKnown: true}
		if err := store.SaveForecastObservation(ctx, observation); err != nil {
			t.Fatal(err)
		}
	}
	f := &forecastTracker{store: store}
	if err := f.scoreRange(ctx, now.Add(-state.ForecastIssueRetention), now); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadForecastErrors(ctx, now.Add(-4*24*time.Hour).UnixMilli(), now.UnixMilli(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("backlog not recovered across pages: %+v", got)
	}
	for _, e := range got {
		if e.PVErrorW != 300 || e.LoadErrorW != 100 {
			t.Fatalf("point error changed: %+v", e)
		}
	}
	if err := f.scoreRange(ctx, now.Add(-state.ForecastIssueRetention), now); err != nil {
		t.Fatalf("restart replay failed: %v", err)
	}
}
