package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

func learningNative(t *testing.T, st *state.Store) *rustForecast {
	t.Helper()
	binary := os.Getenv("FTW_FORECAST_WORKER")
	if binary == "" {
		t.Skip("set FTW_FORECAST_WORKER for native learning integration")
	}
	r, err := newRustForecast(st, binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func learningObservation(site forecastSite, start time.Time) forecasting.Observation {
	return forecasting.Observation{StartMS: start.UnixMilli(), EndMS: start.Add(15 * time.Minute).UnixMilli(), AvailableAtMS: start.Add(15 * time.Minute).UnixMilli(), PVW: 2350.125, PVKnown: true, LoadW: 677.375, LoadKnown: true, Quality: "complete", ConfigVersion: site.Revision}
}

func learningModel(t *testing.T, r *rustForecast, signal string) json.RawMessage {
	t.Helper()
	var saved savedForecastState
	if err := json.Unmarshal(r.Snapshot(), &saved); err != nil {
		t.Fatal(err)
	}
	var models map[string]json.RawMessage
	if err := json.Unmarshal(saved.State, &models); err != nil {
		t.Fatal(err)
	}
	return models[signal]
}

func TestForecastLearningNativeSelectiveResetAndDurableRecovery(t *testing.T) {
	for _, signal := range []string{"pv", "load"} {
		t.Run(signal, func(t *testing.T) {
			st := hostForecastDB(t)
			r := learningNative(t, st)
			site := hostForecastSite()
			start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
			if err := r.Update(context.Background(), site, learningObservation(site, start), nil, false); err != nil {
				t.Fatal(err)
			}
			other := "load"
			if signal == "load" {
				other = "pv"
			}
			originalOther := learningModel(t, r, other)
			cutoff := start.Add(22 * time.Minute).UnixMilli()
			if signal == "pv" {
				site.PVLearningStartedMS = cutoff
			} else {
				site.LoadLearningStartedMS = cutoff
			}
			if err := r.RestartLearning(context.Background(), site, signal); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(originalOther, learningModel(t, r, other)) {
				t.Fatal("reset changed other model")
			}
			if s := r.LearningStatus(site, signal); s.Status != "cold_start" || s.LatestTrainingMS != 0 {
				t.Fatalf("reset status=%+v", s)
			}
			cleared := learningModel(t, r, signal)
			restored := learningNative(t, st)
			if !bytes.Equal(r.Snapshot(), restored.Snapshot()) {
				t.Fatal("DB restore changed state")
			}
			// Replaying an already-consumed pre-reset interval cannot repopulate
			// the cleared signal or publish a partial update of the other model.
			beforeOld := restored.Snapshot()
			if err := restored.Update(context.Background(), site, learningObservation(site, start), nil, false); err == nil {
				t.Fatal("duplicate pre-reset interval unexpectedly accepted")
			}
			if !bytes.Equal(beforeOld, restored.Snapshot()) {
				t.Fatal("failed pre-reset replay changed durable state")
			}
			// A delayed quarter straddles the reset cutoff. Only the other signal learns.
			if err := restored.Update(context.Background(), site, learningObservation(site, start.Add(15*time.Minute)), nil, false); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(cleared, learningModel(t, restored, signal)) {
				t.Fatal("straddling interval repopulated reset model")
			}
			if bytes.Equal(originalOther, learningModel(t, restored, other)) {
				t.Fatal("reset blocked other signal's next interval")
			}
			eligible := learningObservation(site, start.Add(30*time.Minute))
			futureWeather := &state.ForecastPoint{FetchedAtMs: eligible.AvailableAtMS + 1, SolarWm2: hostForecastPtr(500.0)}
			beforeWeather := restored.Snapshot()
			if err := restored.Update(context.Background(), site, eligible, futureWeather, false); err == nil {
				t.Fatal("newer request origin admitted weather unavailable to the observation")
			}
			if !bytes.Equal(beforeWeather, restored.Snapshot()) {
				t.Fatal("rejected weather changed state")
			}
			if err := restored.Update(context.Background(), site, learningObservation(site, start.Add(30*time.Minute)), nil, false); err != nil {
				t.Fatal(err)
			}
			if s := restored.LearningStatus(site, signal); s.Status != "learning" || s.LatestTrainingMS != start.Add(45*time.Minute).UnixMilli() {
				t.Fatalf("post-reset learning=%+v", s)
			}
			learned := restored.Snapshot()
			if err := restored.RestartLearning(context.Background(), site, signal); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(learned, restored.Snapshot()) {
				t.Fatal("durable reset replay erased new evidence")
			}
		})
	}
}

func TestForecastLearningNativeInFlightUpdateCannotUndoReset(t *testing.T) {
	st := hostForecastDB(t)
	r := learningNative(t, st)
	site := hostForecastSite()
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	transport := r.transport.(energyforecast.RoundTripper)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	r.client = energyforecast.NewClient(hostForecastExchange(func(ctx context.Context, p []byte) ([]byte, error) {
		reply, err := transport.RoundTrip(ctx, p)
		var header struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(p, &header)
		if header.Action == "update" {
			once.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}
		return reply, err
	}))
	done := make(chan error, 1)
	go func() { done <- r.Update(context.Background(), site, learningObservation(site, start), nil, false) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("update never reached persistence boundary")
	}
	site.PVLearningStartedMS = start.Add(20 * time.Minute).UnixMilli()
	resetDone := make(chan error, 1)
	go func() { resetDone <- r.RestartLearning(context.Background(), site, "pv") }()
	select {
	case err := <-resetDone:
		close(release)
		t.Fatalf("reset bypassed in-flight update lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-resetDone; err != nil {
		t.Fatal(err)
	}
	reloaded := learningNative(t, st)
	if s := reloaded.LearningStatus(site, "pv"); s.Status != "cold_start" || s.LatestTrainingMS != 0 {
		t.Fatalf("old update restored PV: %+v", s)
	}
	if s := reloaded.LearningStatus(site, "load"); s.LatestTrainingMS != start.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("load history lost: %+v", s)
	}
}
