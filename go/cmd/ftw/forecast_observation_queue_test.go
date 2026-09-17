package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

func observationJobFixture() forecastObservationJob {
	at := time.Now().Add(-time.Hour).Truncate(time.Hour)
	return forecastObservationJob{site: trackerSite(), observation: forecasting.Observation{
		StartMS: at.UnixMilli(), EndMS: at.Add(15 * time.Minute).UnixMilli(), AvailableAtMS: at.Add(15 * time.Minute).UnixMilli(),
		LoadW: 500, PVW: 1000, LoadKnown: true, PVKnown: true, Quality: "complete_balance_v1", ConfigVersion: trackerSite().Revision,
	}}
}

func TestForecastObservationRetainedAcrossRealSQLiteDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	blocker.SetMaxOpenConns(1)
	if _, err := blocker.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec("ROLLBACK")
	f := trackerFixture(time.Now())
	f.store = s
	job := observationJobFixture()
	f.enqueueObservation(job)
	f.flushObservations(context.Background())
	if len(f.pendingObservations) != 1 || f.observationError != "observation_save_pending" {
		t.Fatalf("failed interval lost or hidden: queue=%d error=%q", len(f.pendingObservations), f.observationError)
	}
	if _, err := blocker.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	f.flushObservations(context.Background())
	if len(f.pendingObservations) != 0 || f.observationError != "" {
		t.Fatal("queue did not recover")
	}
	rows, err := s.LoadForecastObservations(context.Background(), job.observation.StartMS, job.observation.EndMS)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, []forecasting.Observation{job.observation}) {
		t.Fatalf("retry changed measured evidence: %+v", rows)
	}
}

func TestForecastObservationRetryPreservesArchiveAndModelStages(t *testing.T) {
	for _, phase := range []string{"ambiguous archive", "model save"} {
		t.Run(phase, func(t *testing.T) {
			f := trackerFixture(time.Now())
			job := observationJobFixture()
			f.enqueueObservation(job)
			saves, updates := 0, 0
			save := func(_ context.Context, o forecasting.Observation) error {
				saves++
				if o != job.observation {
					t.Fatal("retry changed interval")
				}
				if phase == "ambiguous archive" && saves == 1 {
					return context.DeadlineExceeded
				}
				return nil
			}
			update := func(_ context.Context, got forecastObservationJob) error {
				updates++
				if got.observation != job.observation {
					t.Fatal("retry changed training input")
				}
				if phase == "model save" && updates == 1 {
					return errors.New("disk busy")
				}
				return nil
			}
			f.flushObservationsWith(context.Background(), save, update)
			if len(f.pendingObservations) != 1 || f.observationError == "" {
				t.Fatal("failure not retained")
			}
			f.flushObservationsWith(context.Background(), save, update)
			if len(f.pendingObservations) != 0 || f.observationError != "" {
				t.Fatal("recovery did not complete")
			}
			if phase == "model save" && saves != 1 {
				t.Fatal("already committed archive saved again")
			}
		})
	}
}

func TestForecastObservationQueueBoundAndCancellation(t *testing.T) {
	f := trackerFixture(time.Now())
	job := observationJobFixture()
	for i := 0; i <= maxPendingForecastObservations; i++ {
		f.enqueueObservation(job)
	}
	if len(f.pendingObservations) != maxPendingForecastObservations || !f.observationOverflow {
		t.Fatal("overflow unbounded or hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.flushObservationsWith(ctx, func(context.Context, forecasting.Observation) error { t.Fatal("save after cancellation"); return nil }, nil)
	if len(f.pendingObservations) != maxPendingForecastObservations {
		t.Fatal("cancellation dropped pending evidence")
	}
	status := forecasting.LearningStatus{Engine: "energyplan", Status: "ready", LatestTrainingMS: time.Now().UnixMilli()}
	if health, _ := f.learningHealth("load", status); health != "degraded" {
		t.Fatal("queue overflow reported healthy")
	}
}

func TestForecastLearningHealthRequiresCurrentPipelineEvidence(t *testing.T) {
	now := time.Now()
	f := trackerFixture(now)
	status := forecasting.LearningStatus{Engine: "energyplan", Status: "ready", LatestTrainingMS: now.UnixMilli()}
	check := func(want string) {
		t.Helper()
		if health, reason := f.learningHealth("load", status); health != want {
			t.Fatalf("health=%s reason=%s want=%s", health, reason, want)
		}
	}
	check("unknown")
	f.observationCheckedMS, f.observationLoadValid = now.UnixMilli(), true
	check("healthy")
	f.observationError = "model_save_pending"
	check("degraded")
	f.observationError = ""
	f.issueArchiveError = true
	check("degraded")
	f.issueArchiveError = false
	f.observationLoadValid = false
	check("waiting_for_data")
	f.observationLoadValid = true
	status.LatestTrainingMS = now.Add(-3 * time.Hour).UnixMilli()
	check("waiting_for_data")
}

func TestForecastObservationShutdownDrainsWithFreshContext(t *testing.T) {
	s, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := trackerFixture(time.Now())
	f.store = s
	f.enqueueObservation(observationJobFixture())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.runObservations(ctx)
	if len(f.pendingObservations) != 0 {
		t.Fatal("shutdown did not save pending observation")
	}
}

type observationCandidate struct {
	trackingCandidate
	calls int
}

func (c *observationCandidate) Update(context.Context, forecastSite, forecasting.Observation, *state.ForecastPoint, bool) error {
	c.calls++
	return nil
}

func TestForecastObservationOldRevisionCannotReplaceCurrentModel(t *testing.T) {
	f := trackerFixture(time.Now())
	candidate := &observationCandidate{}
	f.candidate = candidate
	job := observationJobFixture()
	job.site.Revision = "previous"
	if err := f.updateObservation(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if candidate.calls != 0 {
		t.Fatal("old configuration changed the current model")
	}
	job.site = trackerSite()
	if err := f.updateObservation(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if candidate.calls != 1 {
		t.Fatal("current observation did not update model")
	}
}
