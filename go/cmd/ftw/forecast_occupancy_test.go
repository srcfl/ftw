package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
)

func TestForecastOccupancySnapshotSurvivesIntentChange(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	tracker := trackerFixture(at)
	away := true
	tracker.away = func(time.Time) bool { return away }
	inputs := tracker.Snapshot(at, trackerWeather(at, at))
	away = false
	slots := trackerSlots(at, 4)
	inputs.Record(slots, slots, "occupancy-frozen", at.UnixMilli())
	job := <-tracker.queue
	if len(job.issue.Occupancy) != 193 {
		t.Fatalf("captured quarter count=%d", len(job.issue.Occupancy))
	}
	for _, row := range job.issue.Occupancy {
		if row.Home || row.AvailableAtMS > job.issue.OriginMS {
			t.Fatalf("live intent or future knowledge entered archive: %+v", row)
		}
		if row.AvailableAtMS > job.issue.LatestInputMS {
			t.Fatal("latest input precedes occupancy feature")
		}
	}
	if err := job.issue.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRustForecastOccupancyRejectsGapBeforeWorker(t *testing.T) {
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	issue := hostForecastIssue(start, start, 1)
	issue.Occupancy = []forecasting.Occupancy{{StartMS: start.UnixMilli(), EndMS: start.Add(15 * time.Minute).UnixMilli(), AvailableAtMS: start.UnixMilli(), Home: false}}
	called := false
	r := &rustForecast{client: energyforecast.NewClient(hostForecastExchange(func(context.Context, []byte) ([]byte, error) { called = true; return nil, nil }))}
	if _, err := r.Predict(context.Background(), hostForecastSite(), issue, nil, nil); err == nil || called {
		t.Fatal("missing archived occupancy defaulted to current home profile")
	}
}

func TestRustForecastNativeOccupancyReplayAfterIntentChange(t *testing.T) {
	binary := os.Getenv("FTW_FORECAST_WORKER")
	if binary == "" {
		t.Skip("set FTW_FORECAST_WORKER for native occupancy replay")
	}
	st := hostForecastDB(t)
	site := hostForecastSite()
	site.HasLocation = false
	worker, err := newRustForecast(st, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	origin := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// Different measured profiles make replacing captured absence observable.
	for day := 16; day > 0; day-- {
		at := origin.AddDate(0, 0, -day)
		away := day%2 == 0
		load := 4000.0
		if away {
			load = 200
		}
		o := forecasting.Observation{StartMS: at.UnixMilli(), EndMS: at.Add(15 * time.Minute).UnixMilli(), AvailableAtMS: at.Add(15 * time.Minute).UnixMilli(), LoadKnown: true, LoadW: load, ConfigVersion: site.Revision, Quality: "complete"}
		if err = worker.Update(context.Background(), site, o, nil, away); err != nil {
			t.Fatal(err)
		}
	}
	frozen := worker.Snapshot()
	issue := hostForecastIssue(origin, origin, 1)
	awayAtIssue := map[int64]bool{}
	for i := 0; i < 4; i++ {
		at := origin.Add(time.Duration(i) * 15 * time.Minute).UnixMilli()
		awayAtIssue[at] = true
	}
	first, err := worker.Predict(context.Background(), site, issue, frozen, awayAtIssue)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range first.Occupancy {
		if row.AvailableAtMS > first.LatestInputMS {
			t.Fatal("candidate latest input precedes occupancy feature")
		}
	}
	applyForecastBands(&first, nil)
	if err = st.SaveForecastIssue(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	archived, err := st.LoadForecastIssues(context.Background(), 0, time.Now().Add(time.Hour).UnixMilli(), 10)
	if err != nil || len(archived) != 1 {
		t.Fatalf("archive read: %d %v", len(archived), err)
	}
	replayIssue := archived[0]
	var opaque json.RawMessage
	for _, model := range replayIssue.Models {
		if model.Name == "energyplan" {
			opaque, err = st.LoadForecastModelState(context.Background(), model.StateID)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(opaque) == 0 {
		t.Fatal("missing archived native model")
	}
	replaySite := forecastSite{SiteID: replayIssue.Site.SiteID, Revision: replayIssue.ConfigVersion, LearningRevision: replayIssue.Site.LearningRevision, Timezone: replayIssue.Site.Timezone, HasLocation: replayIssue.Site.HasLocation, Latitude: replayIssue.Site.Latitude, Longitude: replayIssue.Site.Longitude}
	replayRaw, err := json.Marshal(savedForecastState{SiteID: replaySite.SiteID, ConfigRevision: rustConfigRevision(replaySite), LatestAvailableMS: replayIssue.Models[0].UpdatedAtMS, State: opaque})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newRustForecast(st, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	// The caller now says home. Archived features must take precedence.
	replay, err := restarted.Predict(context.Background(), replaySite, replayIssue, replayRaw, map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	applyForecastBands(&replay, nil)
	if !reflect.DeepEqual(first.Series, replay.Series) || !reflect.DeepEqual(first.Occupancy, replay.Occupancy) {
		t.Fatal("archived absence changed after restart/current intent change")
	}
	homeIssue := issue
	home, err := restarted.Predict(context.Background(), site, homeIssue, frozen, map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(home.Series[0].Points[0].LoadW, first.Series[0].Points[0].LoadW) {
		t.Fatal("fixture did not distinguish home and away predictions")
	}
}

func TestForecastPendingIdentityCannotUseSavedModels(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	f := trackerFixture(at)
	pending := trackerSite()
	pending.IdentityPending = true
	f.site = func() forecastSite { return pending }
	f.configMu = &sync.RWMutex{}
	refreshes := 0
	f.refreshIdentity = func() {
		if !f.configMu.TryLock() {
			t.Fatal("identity refresh runs under config read lock")
		}
		f.configMu.Unlock()
		refreshes++
	}
	candidateRead := false
	f.candidate = &trackingCandidate{onSnapshot: func() { candidateRead = true }}
	in := f.Snapshot(at, trackerWeather(at, at))
	if in.Weather == nil || len(in.Weather) != 0 || in.PV != nil || in.Load != nil || in.Record != nil || candidateRead {
		t.Fatal("unconfirmed hardware used or archived a saved model")
	}
	// Pending startup must not touch live telemetry or the archive either.
	f.tele = nil
	f.store = nil
	f.observe(context.Background(), at)
	if refreshes != 2 {
		t.Fatalf("identity refreshes=%d want2", refreshes)
	}
}
