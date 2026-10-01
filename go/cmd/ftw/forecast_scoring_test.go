package main

import (
	"context"
	"errors"
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
	if err := newForecastScorePass(now.Add(-state.ForecastIssueRetention), now).run(ctx, store); err != nil {
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
	if err := newForecastScorePass(now.Add(-state.ForecastIssueRetention), now).run(ctx, store); err != nil {
		t.Fatalf("restart replay failed: %v", err)
	}
}

type interruptedScoreStore struct {
	*state.Store
	stage         string
	failed        bool
	saves, prunes int
	cursors       []state.ForecastIssueCursor
}

func (s *interruptedScoreStore) LoadForecastScorePage(ctx context.Context, since, until int64, cursor state.ForecastIssueCursor, limit int) ([]forecasting.Issue, state.ForecastIssueCursor, error) {
	s.cursors = append(s.cursors, cursor)
	if s.stage == "read" && s.saves == 1 && !s.failed {
		s.failed = true
		return nil, cursor, context.DeadlineExceeded
	}
	return s.Store.LoadForecastScorePage(ctx, since, until, cursor, limit)
}

func (s *interruptedScoreStore) SaveForecastErrors(ctx context.Context, samples []forecasting.ErrorSample, now int64) error {
	if s.stage == "save" && s.saves == 1 && !s.failed {
		s.failed = true
		return context.DeadlineExceeded
	}
	if err := s.Store.SaveForecastErrors(ctx, samples, now); err != nil {
		return err
	}
	s.saves++
	return nil
}

func (s *interruptedScoreStore) PruneForecastErrors(ctx context.Context, now int64) error {
	s.prunes++
	if s.stage == "prune" && s.saves == 1 && !s.failed {
		s.failed = true
		return context.DeadlineExceeded
	}
	return s.Store.PruneForecastErrors(ctx, now)
}

func TestForecastScoringResumesCommittedPages(t *testing.T) {
	for _, stage := range []string{"read", "save", "prune"} {
		t.Run(stage, func(t *testing.T) {
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
			start := now.Add(-2 * time.Hour)
			band := forecasting.Band{LowW: 0, HighW: 5000, Method: forecasting.BandMethodColdStart}
			point := forecasting.Point{StartMS: start.UnixMilli(), EndMS: start.Add(time.Hour).UnixMilli(), PVW: 1000, LoadW: 500, PVKnown: true, LoadKnown: true, PVQuality: "forecast", LoadQuality: "forecast", PVBand: band, LoadBand: band, NetBand: band}
			latePoint := point
			latePoint.StartMS, latePoint.EndMS = now.Add(-time.Hour).UnixMilli(), now.UnixMilli()
			for i := 0; i < 9; i++ {
				origin := start.Add(-90 * time.Minute).Add(time.Duration(i) * time.Second).UnixMilli()
				issue := forecasting.Issue{Schema: forecasting.Schema, ID: fmt.Sprintf("resume-%d", i), OriginMS: origin, IssuedAtMS: origin, LatestInputMS: origin, ConfigVersion: "cfg", Series: []forecasting.Series{{Name: "champion", ModelVersion: "v1", Points: []forecasting.Point{point, latePoint}}}}
				if err := store.SaveForecastIssue(ctx, issue); err != nil {
					t.Fatal(err)
				}
			}
			observation := forecasting.Observation{StartMS: point.StartMS, EndMS: point.EndMS, AvailableAtMS: point.EndMS, ConfigVersion: "cfg", Quality: "complete", PVW: 1300, LoadW: 600, PVKnown: true, LoadKnown: true}
			if err := store.SaveForecastObservation(ctx, observation); err != nil {
				t.Fatal(err)
			}
			faults := &interruptedScoreStore{Store: store, stage: stage}
			pass := newForecastScorePass(now.Add(-state.ForecastIssueRetention), now)
			if err := pass.run(ctx, faults); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("first attempt: %v", err)
			}
			if pass.pages != 1 || pass.cursor.ID != "resume-3" || faults.saves != 1 {
				t.Fatalf("failed step lost committed progress: pages=%d cursor=%+v saves=%d", pass.pages, pass.cursor, faults.saves)
			}
			if stage == "prune" {
				for i := 0; i < 3; i++ {
					faults.failed = false
					if err := pass.run(ctx, faults); !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("maintenance retry: %v", err)
					}
					if !pass.maintenance || pass.pages != 1 || faults.saves != 1 {
						t.Fatal("failed maintenance allowed the score archive to keep growing")
					}
				}
			}
			tracker := &forecastTracker{store: store}
			tracker.refreshEvidence(ctx, now)
			if len(tracker.errors) != 1 || tracker.errors[0].IssueID != "resume-3" {
				t.Fatalf("committed partial evidence unavailable: %+v", tracker.errors)
			}
			// A delayed observation arrives while the pass is paused. It belongs
			// to the next overlap, not just the unread tail of this pass.
			observation.StartMS, observation.EndMS = latePoint.StartMS, latePoint.EndMS
			observation.AvailableAtMS = now.UnixMilli()
			if err := store.SaveForecastObservation(ctx, observation); err != nil {
				t.Fatal(err)
			}
			reads := len(faults.cursors)
			if err := pass.run(ctx, faults); err != nil {
				t.Fatal(err)
			}
			if faults.cursors[reads].ID != "resume-3" || pass.pages != 3 || faults.saves != 3 {
				t.Fatalf("retry replayed committed pages: reads=%+v pages=%d saves=%d", faults.cursors, pass.pages, faults.saves)
			}
			got, err := store.LoadForecastErrors(ctx, start.UnixMilli(), now.UnixMilli(), false)
			if err != nil || len(got) != 1 || got[0].IssueID != "resume-8" {
				t.Fatalf("retry result: %+v %v", got, err)
			}
			if err := newForecastScorePass(pass.until.Add(-2*time.Hour), now.Add(time.Minute)).run(ctx, store); err != nil {
				t.Fatal(err)
			}
			got, err = store.LoadForecastErrors(ctx, start.UnixMilli(), now.UnixMilli(), false)
			if err != nil || len(got) != 2 || got[1].IssueID != "resume-8" {
				t.Fatalf("late observation lost: %+v %v", got, err)
			}
		})
	}
}
