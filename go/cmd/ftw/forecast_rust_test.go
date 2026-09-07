package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
)

type hostForecastExchange func(context.Context, []byte) ([]byte, error)

func (f hostForecastExchange) RoundTrip(ctx context.Context, p []byte) ([]byte, error) {
	return f(ctx, p)
}
func hostForecastPtr[T any](v T) *T { return &v }
func hostForecastSite() forecastSite {
	return forecastSite{SiteID: "host-test", Revision: "cfg-1", Latitude: 57, Longitude: 15, HasLocation: true, Timezone: "Europe/Stockholm"}
}
func hostForecastIssue(start time.Time, origin time.Time, hours int) forecasting.Issue {
	issue := forecasting.Issue{Schema: forecasting.Schema, ID: "champion-test", DecisionID: "decision-test", OriginMS: origin.UnixMilli(), IssuedAtMS: origin.UnixMilli(), ConfigVersion: "cfg-1"}
	series := forecasting.Series{Name: "champion", ModelVersion: "test"}
	for i := 0; i < hours; i++ {
		at := start.Add(time.Duration(i) * time.Hour).UnixMilli()
		series.Points = append(series.Points, forecasting.Point{StartMS: at, EndMS: at + 3600000, PVKnown: true, LoadKnown: true, PVQuality: "cold_start", LoadQuality: "cold_start"})
		issue.Weather = append(issue.Weather, forecasting.Weather{StartMS: at, EndMS: at + 3600000, AvailableAtMS: origin.Add(-time.Hour).UnixMilli(), Source: "test", GHIWm2: hostForecastPtr(500.0)})
	}
	issue.Series = []forecasting.Series{series}
	applyForecastBands(&issue, nil)
	return issue
}
func hostForecastReply(payload []byte) (map[string]any, map[string]any) {
	var req map[string]any
	_ = json.Unmarshal(payload, &req)
	reply := map[string]any{"ok": true, "model_revision": 1, "latest_input_ms": map[string]any{"pv": nil, "load": nil}, "latest_training_ms": map[string]any{"pv": nil, "load": nil}, "latest_available_at_ms": map[string]any{"pv": nil, "load": nil}}
	for _, key := range []string{"op", "version", "action", "request_id", "site_id", "config_revision", "origin_ms"} {
		reply[key] = req[key]
	}
	if req["action"] == "update" {
		reply["state"] = map[string]any{"observed_at": req["origin_ms"]}
		reply["updates"] = map[string]any{"pv": map[string]any{"applied": 1, "skipped": 0}, "load": map[string]any{"applied": 1, "skipped": 0}}
		return req, reply
	}
	var predictions []any
	for _, raw := range req["horizon"].([]any) {
		slot := raw.(map[string]any)
		quarter := (int64(slot["valid_start_ms"].(float64)) % 3600000) / 900000
		pv := float64((quarter + 1) * 100)
		quality := "ready"
		if quarter == 2 {
			quality = "learning"
		}
		predictions = append(predictions, map[string]any{"valid_start_ms": slot["valid_start_ms"], "valid_end_ms": slot["valid_end_ms"], "pv": map[string]any{"known": true, "point_w": pv, "lower_w": pv - 50, "upper_w": pv + 50, "quality": quality, "uncertainty": "provisional", "coverage": 0.5}, "load": map[string]any{"known": true, "point_w": pv + 1000, "lower_w": pv + 900, "upper_w": pv + 1100, "quality": "cold_start", "uncertainty": "provisional", "coverage": 0}})
	}
	reply["predictions"] = predictions
	return req, reply
}
func hostForecastDB(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "forecast.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err = st.InitForecastArchive(context.Background()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRustForecastHostQuarterEnergyAndPartialHour(t *testing.T) {
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	issue := hostForecastIssue(start, start.Add(7*time.Minute), 2)
	var captured energyforecast.PredictRequest
	r := &rustForecast{client: energyforecast.NewClient(hostForecastExchange(func(_ context.Context, p []byte) ([]byte, error) {
		if err := json.Unmarshal(p, &captured); err != nil {
			t.Fatal(err)
		}
		_, reply := hostForecastReply(p)
		return json.Marshal(reply)
	}))}
	away := map[int64]bool{start.Add(75 * time.Minute).UnixMilli(): true}
	got, err := r.Predict(context.Background(), hostForecastSite(), issue, nil, away)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured.Horizon) != 7 {
		t.Fatalf("future full quarters=%d want7", len(captured.Horizon))
	}
	for _, slot := range captured.Horizon {
		if slot.ValidStartMs < issue.OriginMS || slot.ValidStartMs%900000 != 0 || slot.ValidEndMs-slot.ValidStartMs != 900000 {
			t.Fatalf("invalid candidate quarter: %+v", slot)
		}
		if slot.Home == nil || *slot.Home == away[slot.ValidStartMs] {
			t.Fatal("occupancy did not follow quarter")
		}
		if slot.WeatherAvailableAtMs == nil || *slot.WeatherAvailableAtMs > issue.OriginMS {
			t.Fatal("weather availability lost")
		}
	}
	points := got.Series[0].Points
	if len(points) != 1 || points[0].StartMS != start.Add(time.Hour).UnixMilli() {
		t.Fatalf("partial current hour was published: %+v", points)
	}
	p := points[0]
	if p.PVW != 250 || p.LoadW != 1250 {
		t.Fatalf("quarter power not energy mean: PV=%v load=%v", p.PVW, p.LoadW)
	}
	if !p.PVKnown || !p.LoadKnown || p.PVQuality != "learning" || p.LoadQuality != "cold_start" {
		t.Fatalf("known cold-start or weakest quality lost: %+v", p)
	}
	if p.ModelPV == nil || p.ModelLoad == nil || p.ModelPV.LowerW != 200 || p.ModelPV.UpperW != 300 || p.ModelPV.Uncertainty != "provisional" || p.ModelLoad.Coverage != 0 {
		t.Fatalf("provisional model evidence lost: %+v %+v", p.ModelPV, p.ModelLoad)
	}
	applyForecastBands(&got, nil)
	if got.Series[0].Points[0].PVBand.Method != forecasting.BandMethodColdStart || got.Series[0].Points[0].PVBand.Samples != 0 {
		t.Fatal("model bounds claimed empirical calibration")
	}
	if err = got.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRustForecastHostDSTQuarterFeatures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   time.Time
		minutes []int
	}{{"spring", time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC), []int{60, 75, 90, 105, 180, 195, 210, 225}}, {"autumn", time.Date(2025, 10, 26, 0, 0, 0, 0, time.UTC), []int{120, 135, 150, 165, 120, 135, 150, 165}}} {
		t.Run(tc.name, func(t *testing.T) {
			var captured energyforecast.PredictRequest
			r := &rustForecast{client: energyforecast.NewClient(hostForecastExchange(func(_ context.Context, p []byte) ([]byte, error) {
				json.Unmarshal(p, &captured)
				_, reply := hostForecastReply(p)
				return json.Marshal(reply)
			}))}
			_, err := r.Predict(context.Background(), hostForecastSite(), hostForecastIssue(tc.start, tc.start, 2), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(captured.Horizon) != len(tc.minutes) {
				t.Fatal("DST changed UTC quarter count")
			}
			for i, s := range captured.Horizon {
				if s.LocalMinute != tc.minutes[i] || s.LocalWeekday != 6 || s.LocalDay != tc.start.Unix()/86400 || s.ValidStartMs != tc.start.Add(time.Duration(i)*15*time.Minute).UnixMilli() {
					t.Fatalf("DST quarter%d: %+v", i, s)
				}
			}
		})
	}
}

func TestRustForecastHostFrozenStateAndConfigBoundary(t *testing.T) {
	st := hostForecastDB(t)
	site := hostForecastSite()
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	var requests []energyforecast.PredictRequest
	r := &rustForecast{store: st, client: energyforecast.NewClient(hostForecastExchange(func(_ context.Context, p []byte) ([]byte, error) {
		req, reply := hostForecastReply(p)
		if req["action"] == "predict" {
			var captured energyforecast.PredictRequest
			json.Unmarshal(p, &captured)
			requests = append(requests, captured)
		}
		return json.Marshal(reply)
	}))}
	observe := func(at time.Time) {
		t.Helper()
		if err := r.Update(context.Background(), site, forecasting.Observation{StartMS: at.Add(-15 * time.Minute).UnixMilli(), EndMS: at.UnixMilli(), AvailableAtMS: at.UnixMilli(), PVKnown: true, PVW: 2000, LoadKnown: true, LoadW: 1000}, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	observe(start)
	frozen := r.Snapshot()
	observe(start.Add(time.Hour))
	issue := hostForecastIssue(start, start, 1)
	if _, err := r.Predict(context.Background(), site, issue, frozen, nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("prediction requests=%d want1", len(requests))
	}
	var frozenState map[string]int64
	if err := json.Unmarshal(requests[0].State, &frozenState); err != nil {
		t.Fatal(err)
	}
	if frozenState["observed_at"] != start.UnixMilli() {
		t.Fatalf("new input leaked into frozen request: %s", requests[0].State)
	}
	if _, err := r.Predict(context.Background(), site, issue, r.Snapshot(), nil); err == nil {
		t.Fatal("future state accepted")
	}
	if len(requests) != 1 {
		t.Fatal("future state reached worker")
	}
	site.Revision = "cfg-2"
	issue.ConfigVersion = site.Revision
	if _, err := r.Predict(context.Background(), site, issue, frozen, nil); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || len(requests[1].State) != 0 || requests[1].ConfigRevision != "cfg-2" {
		t.Fatal("old config state crossed model boundary")
	}
}

func TestRustForecastHostNativePersistenceAndArchive(t *testing.T) {
	binary := os.Getenv("FTW_FORECAST_WORKER")
	if binary == "" {
		t.Skip("set FTW_FORECAST_WORKER for native host integration")
	}
	st := hostForecastDB(t)
	site := hostForecastSite()
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	r, err := newRustForecast(st, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	o := forecasting.Observation{StartMS: start.Add(-15 * time.Minute).UnixMilli(), EndMS: start.UnixMilli(), AvailableAtMS: start.UnixMilli(), LoadW: 1000, LoadKnown: true, PVW: 2000, PVKnown: true, Quality: "complete", ConfigVersion: site.Revision}
	weather := &state.ForecastPoint{SlotTsMs: o.StartMS, SlotLenMin: 15, FetchedAtMs: start.Add(-time.Hour).UnixMilli(), Source: "test", SolarWm2: hostForecastPtr(500.0)}
	if err = r.Update(context.Background(), site, o, weather, false); err != nil {
		t.Fatal(err)
	}
	frozen := r.Snapshot()
	issue := hostForecastIssue(start, start, 1)
	first, err := r.Predict(context.Background(), site, issue, frozen, nil)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newRustForecast(st, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if string(restarted.Snapshot()) != string(frozen) {
		t.Fatal("constructor failed to reload saved model")
	}
	second, err := restarted.Predict(context.Background(), site, issue, restarted.Snapshot(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Series, second.Series) || !reflect.DeepEqual(first.Models, second.Models) {
		t.Fatal("native host replay changed forecast or frozen model")
	}
	applyForecastBands(&first, nil)
	if err = first.Validate(); err != nil {
		t.Fatal(err)
	}
	if err = st.SaveForecastIssue(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	archived, err := st.LoadForecastIssues(context.Background(), 0, time.Now().Add(time.Hour).UnixMilli(), 10)
	if err != nil || len(archived) != 1 {
		t.Fatalf("native issue archive: count=%d err=%v", len(archived), err)
	}
	if !reflect.DeepEqual(archived[0].Series, first.Series) {
		t.Fatal("archive changed model predictions or evidence")
	}
}

func TestRustForecastHostUnknownQuarterCannotBecomeKnownHour(t *testing.T) {
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	r := &rustForecast{client: energyforecast.NewClient(hostForecastExchange(func(_ context.Context, p []byte) ([]byte, error) {
		_, reply := hostForecastReply(p)
		predictions := reply["predictions"].([]any)
		predictions[1].(map[string]any)["pv"] = map[string]any{"known": false, "quality": "unknown", "uncertainty": "unavailable", "coverage": 0}
		return json.Marshal(reply)
	}))}
	out, err := r.Predict(context.Background(), hostForecastSite(), hostForecastIssue(start, start, 1), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := out.Series[0].Points[0]
	if p.PVKnown || p.PVQuality != "unknown" || p.ModelPV != nil {
		t.Fatalf("missing quarter acquired numeric evidence: %+v", p)
	}
	if !p.LoadKnown || p.LoadQuality != "cold_start" {
		t.Fatal("independent known cold load erased by unknown PV")
	}
}

func TestRustForecastHostUpdateZeroAvailabilityAndDeadline(t *testing.T) {
	st := hostForecastDB(t)
	site := hostForecastSite()
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	var captured energyforecast.UpdateRequest
	calls := 0
	r := &rustForecast{store: st, client: energyforecast.NewClient(hostForecastExchange(func(_ context.Context, p []byte) ([]byte, error) {
		calls++
		if err := json.Unmarshal(p, &captured); err != nil {
			t.Fatal(err)
		}
		_, reply := hostForecastReply(p)
		return json.Marshal(reply)
	}))}
	observation := forecasting.Observation{StartMS: start.Add(-15 * time.Minute).UnixMilli(), EndMS: start.UnixMilli(), AvailableAtMS: start.UnixMilli(), PVKnown: true, PVW: 0, LoadKnown: false}
	if err := r.Update(context.Background(), site, observation, nil, true); err != nil {
		t.Fatal(err)
	}
	sample := captured.Observations[0]
	if sample.PVAvailableW == nil || *sample.PVAvailableW != 0 || sample.PVQuality != energyforecast.QualityGood || sample.HouseholdLoadW != nil || sample.LoadQuality != energyforecast.QualityMissing || sample.Home == nil || *sample.Home {
		t.Fatalf("zero/missing/away labels changed: %+v", sample)
	}
	frozen := string(r.Snapshot())
	futureWeather := &state.ForecastPoint{FetchedAtMs: start.Add(time.Second).UnixMilli(), SolarWm2: hostForecastPtr(500.0)}
	if err := r.Update(context.Background(), site, observation, futureWeather, false); err == nil {
		t.Fatal("future weather accepted")
	}
	if calls != 1 || string(r.Snapshot()) != frozen {
		t.Fatal("invalid update changed state or reached worker")
	}
	r.client = energyforecast.NewClient(hostForecastExchange(func(ctx context.Context, _ []byte) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.Update(ctx, site, observation, nil, false); err == nil {
		t.Fatal("deadline ignored")
	}
	if string(r.Snapshot()) != frozen {
		t.Fatal("timed-out update replaced frozen state")
	}
}

func TestRustForecastHostPreservesIndependentInputClocks(t *testing.T) {
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	origin := start.UnixMilli()
	site := hostForecastSite()
	wantInput := energyforecast.LatestInput{PV: hostForecastPtr(origin - 1800000), Load: hostForecastPtr(origin - 900000)}
	wantTraining := energyforecast.LatestInput{PV: hostForecastPtr(origin - 2700000), Load: hostForecastPtr(origin - 900000)}
	wantAvailable := energyforecast.LatestInput{PV: hostForecastPtr(origin - 1500000), Load: hostForecastPtr(origin - 840000)}
	r := &rustForecast{client: energyforecast.NewClient(hostForecastExchange(func(_ context.Context, p []byte) ([]byte, error) {
		_, reply := hostForecastReply(p)
		reply["model_revision"] = 17
		reply["latest_input_ms"] = wantInput
		reply["latest_training_ms"] = wantTraining
		reply["latest_available_at_ms"] = wantAvailable
		return json.Marshal(reply)
	}))}
	raw, err := json.Marshal(savedForecastState{SiteID: site.SiteID, ConfigRevision: site.Revision, LatestAvailableMS: *wantAvailable.Load, State: json.RawMessage(`{"opaque":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := r.Predict(context.Background(), site, hostForecastIssue(start, start, 1), raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Revision  uint64                     `json:"model_revision"`
		Input     energyforecast.LatestInput `json:"latest_input_ms"`
		Training  energyforecast.LatestInput `json:"latest_training_ms"`
		Available energyforecast.LatestInput `json:"latest_available_at_ms"`
	}
	found := false
	for _, m := range out.Models {
		if m.Name == "energyplan_metadata" {
			found = true
			if err = json.Unmarshal(m.State, &evidence); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !found || evidence.Revision != 17 || !reflect.DeepEqual(evidence.Input, wantInput) || !reflect.DeepEqual(evidence.Training, wantTraining) || !reflect.DeepEqual(evidence.Available, wantAvailable) {
		t.Fatalf("PV/load consumed, learned and availability clocks conflated: %+v", evidence)
	}
	applyForecastBands(&out, nil)
	if err = out.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRustForecastHostEvaluationConfigKeepsLearningState(t *testing.T) {
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	site := hostForecastSite()
	site.LearningRevision = "electrical-1"
	var states []json.RawMessage
	r := &rustForecast{client: energyforecast.NewClient(hostForecastExchange(func(_ context.Context, p []byte) ([]byte, error) {
		var req energyforecast.PredictRequest
		if err := json.Unmarshal(p, &req); err != nil {
			t.Fatal(err)
		}
		states = append(states, req.State)
		_, reply := hostForecastReply(p)
		return json.Marshal(reply)
	}))}
	raw, _ := json.Marshal(savedForecastState{SiteID: site.SiteID, ConfigRevision: site.LearningRevision, LatestAvailableMS: start.Add(-time.Hour).UnixMilli(), State: json.RawMessage(`{"opaque":"learned"}`)})
	issue := hostForecastIssue(start, start, 1)
	site.Revision = "evaluation-provider-2"
	issue.ConfigVersion = site.Revision
	if _, err := r.Predict(context.Background(), site, issue, raw, nil); err != nil {
		t.Fatal(err)
	}
	site.LearningRevision = "electrical-2"
	if _, err := r.Predict(context.Background(), site, issue, raw, nil); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || len(states[0]) == 0 || len(states[1]) != 0 {
		t.Fatalf("evaluation and learning revision boundaries confused: %q", states)
	}
}
