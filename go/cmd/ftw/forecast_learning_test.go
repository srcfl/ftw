package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/loadmodel"
	"github.com/srcfl/ftw/go/internal/pvmodel"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func learningTracker(st *state.Store, site *forecastSite, at time.Time, r *rustForecast) *forecastTracker {
	tel := telemetry.NewStore()
	f := &forecastTracker{store: st, tele: tel, site: func() forecastSite { return *site }, clock: func() time.Time { return at },
		pv: pvmodel.NewService(st, tel, func(time.Time) float64 { return 800 }, nil, 8000), load: loadmodel.NewService(st, tel, "site", 4000, 0)}
	if r != nil {
		f.candidate = r
	}
	return f
}

func TestForecastLearningNativePendingExchangeRecoversOnlySelectedSignal(t *testing.T) {
	st := hostForecastDB(t)
	r := learningNative(t, st)
	site := hostForecastSite()
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	if err := r.Update(context.Background(), site, learningObservation(site, at), nil, false); err != nil {
		t.Fatal(err)
	}
	f := learningTracker(st, &site, at.Add(22*time.Minute), r)
	transport := r.transport.(energyforecast.RoundTripper)
	var fail atomic.Bool
	fail.Store(true)
	r.client = energyforecast.NewClient(hostForecastExchange(func(ctx context.Context, p []byte) ([]byte, error) {
		var header struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(p, &header)
		if header.Action == "reset" && fail.Load() {
			return nil, errors.New("injected worker exchange failure")
		}
		return transport.RoundTrip(ctx, p)
	}))
	if err := f.RestartLearning(context.Background(), "pv"); err == nil {
		t.Fatal("failed worker reset reported success")
	}
	if _, ok := st.LoadConfig(forecastLearningKey); !ok {
		t.Fatal("reset intent was not durable before worker call")
	}
	if s := f.LearningStatus("pv"); s.Status != "unavailable" {
		t.Fatalf("pending PV=%+v", s)
	}
	if s := f.LearningStatus("load"); s.Status != "learning" {
		t.Fatalf("pending PV hid load=%+v", s)
	}
	if f.pv.LearningStartedMS() != at.Add(22*time.Minute).UnixMilli() {
		t.Fatal("legacy fallback retained old epoch")
	}
	fail.Store(false)
	f.Snapshot(time.Now(), nil)
	if s := f.LearningStatus("pv"); s.Status != "cold_start" {
		t.Fatalf("automatic reconciliation left PV pending=%+v", s)
	}
	if s := f.LearningStatus("load"); s.Status != "learning" {
		t.Fatalf("recovery changed load=%+v", s)
	}
	restored := learningNative(t, st)
	if s := restored.LearningStatus(f.learningSiteLocked(site), "pv"); s.Status != "cold_start" {
		t.Fatalf("recovered reset not persisted=%+v", s)
	}
}

func TestForecastLearningIdentityReadyAppliesSavedIntentBeforeCapture(t *testing.T) {
	st := hostForecastDB(t)
	site := hostForecastSite()
	site.IdentityPending = true
	at := time.Now().Add(-time.Minute)
	cutoff := at.UnixMilli()
	data, _ := json.Marshal(forecastLearningPeriods{ConfigRevision: rustConfigRevision(site), PVMS: cutoff})
	if err := st.SaveConfig(forecastLearningKey, string(data)); err != nil {
		t.Fatal(err)
	}
	f := learningTracker(st, &site, time.Now(), nil)
	if err := f.restoreLearning(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.pv.LearningStartedMS() != 0 {
		t.Fatal("identity-pending startup changed model")
	}
	site.IdentityPending = false
	f.Snapshot(time.Now(), nil)
	if f.pv.LearningStartedMS() != cutoff {
		t.Fatal("first capture after identity resolution missed durable reset")
	}
	if f.load.LearningStartedMS() != 0 {
		t.Fatal("PV reset changed load epoch")
	}
	restored := learningTracker(st, &site, time.Now(), nil)
	if restored.pv.LearningStartedMS() != cutoff {
		t.Fatal("legacy reset did not survive DB reload")
	}
}

func TestForecastLearningNativeDisabledPVDoesNotPersistIntentOrBlockLoad(t *testing.T) {
	st := hostForecastDB(t)
	r := learningNative(t, st)
	site := hostForecastSite()
	site.HasLocation = false
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	f := learningTracker(st, &site, at.Add(22*time.Minute), r)
	if err := f.RestartLearning(context.Background(), "pv"); err == nil {
		t.Fatal("disabled native PV reset accepted")
	}
	if _, ok := st.LoadConfig(forecastLearningKey); ok {
		t.Fatal("disabled PV reset saved a blocking intent")
	}
	if s := f.LearningStatus("pv"); s.ResetAvailable {
		t.Fatalf("disabled PV reset advertised: %+v", s)
	}
	// An older saved PV intent must not block updates in a load-only model.
	site.PVLearningStartedMS = at.Add(10 * time.Minute).UnixMilli()
	if err := r.Update(context.Background(), site, learningObservation(site, at), nil, false); err != nil {
		t.Fatal(err)
	}
	if s := r.LearningStatus(site, "load"); s.Status != "learning" {
		t.Fatalf("disabled PV blocked load=%+v", s)
	}
}

func TestForecastLearningCalibrationDropsPVAndNetRetainsLoadAndArchive(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	var history []forecasting.ErrorSample
	var truth []forecasting.Observation
	for day := 0; day < 7; day++ {
		for hour := 0; hour < 8; hour++ {
			start := base + int64(day*24+hour)*3600000
			origin := start - 2*3600000
			p := forecasting.Point{StartMS: start, EndMS: start + 3600000, PVW: 2000, LoadW: 1000, PVKnown: true, LoadKnown: true, PVQuality: "ready", LoadQuality: "ready", PVBand: forecasting.Band{Method: forecasting.BandMethodColdStart}, LoadBand: forecasting.Band{Method: forecasting.BandMethodColdStart}, NetBand: forecasting.Band{Method: forecasting.BandMethodColdStart}}
			e := forecasting.ErrorSample{Series: "energyplan", ConfigVersion: "cfg", IssueID: fmt.Sprintf("%d-%d", day, hour), OriginMS: origin, IssuedAtMS: origin, StartMS: start, EndMS: start + 3600000, AvailableAtMS: start + 3600000, Lead: forecasting.LeadBucket(origin, start), PVErrorW: float64(hour + day), LoadErrorW: float64(hour - day), PVKnown: true, LoadKnown: true, Prediction: p}
			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
			history = append(history, e)
			truth = append(truth, forecasting.Observation{StartMS: start, EndMS: start + 3600000, PVKnown: true, LoadKnown: true})
		}
	}
	archived := append([]forecasting.ErrorSample(nil), history...)
	rawTruth := append([]forecasting.Observation(nil), truth...)
	cutoff := base + 8*24*3600000
	before := forecasting.NewCalibrator(history, "cfg", cutoff)
	filtered, observations := learningEvidence(history, truth, cutoff, 0)
	after := forecasting.NewCalibrator(filtered, "cfg", cutoff)
	for _, signal := range []string{"pv", "net", "load"} {
		previous := before.Band("energyplan", signal, cutoff+2*3600000, 1000)
		got := after.Band("energyplan", signal, cutoff+2*3600000, 1000)
		if previous.Method != forecasting.BandMethodEmpirical {
			t.Fatalf("fixture not calibrated for %s: %+v", signal, previous)
		}
		if signal == "load" {
			if got != previous {
				t.Fatalf("PV reset changed load calibration: %+v %+v", previous, got)
			}
		} else if got.Samples != 0 {
			t.Fatalf("%s retained pre-reset errors: %+v", signal, got)
		}
	}
	if !reflect.DeepEqual(history, archived) || !reflect.DeepEqual(truth, rawTruth) {
		t.Fatal("reset rewrote archived evidence")
	}
	if observations[0].PVKnown || !observations[0].LoadKnown {
		t.Fatal("observation evidence reset crossed signals")
	}
}
