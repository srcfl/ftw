package main

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func primaryFixture(at time.Time, exchange hostForecastExchange) *forecastTracker {
	f := trackerFixture(at)
	f.site = hostForecastSite
	f.candidate = &rustForecast{client: energyforecast.NewClient(exchange)}
	return f
}

func primaryReply(_ context.Context, payload []byte) ([]byte, error) {
	_, reply := primaryHostForecastReply(payload)
	return json.Marshal(reply)
}

// Selection tests use a learned load; raw host tests retain cold-start evidence.
func primaryHostForecastReply(payload []byte) (map[string]any, map[string]any) {
	req, reply := hostForecastReply(payload)
	for _, row := range reply["predictions"].([]any) {
		load := row.(map[string]any)["load"].(map[string]any)
		load["quality"], load["coverage"] = "learning", 0.5
	}
	return req, reply
}

func primarySeries(t *testing.T, issue forecasting.Issue, name string) forecasting.Series {
	t.Helper()
	for _, s := range issue.Series {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("missing %s", name)
	return forecasting.Series{}
}

func TestForecastPrimaryRustControlsSlotsAndArchivesFrozenLegacy(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	f := primaryFixture(at, primaryReply)
	weather := trackerWeather(at, at)
	in := f.Snapshot(at, weather)
	legacy := trackerSlots(at, 4)
	base := in.Resolve(context.Background(), legacy)
	if base[0].PVW != -100 || base[0].LoadW != 1100 || base[2].PVW != -300 {
		t.Fatalf("Rust did not replace actual slots: %+v", base)
	}
	if in.PVUncertaintyW != 0 || in.PVRelativeUncertainty != 0 {
		t.Fatal("primary inherited legacy uncertainty scalars")
	}
	planning := append([]mpc.Slot(nil), base...)
	in.Risk(base, planning, 1)
	if planning[0].PVW != -50 || planning[2].PVW != -250 {
		t.Fatalf("risk did not use Rust provisional range: %+v", planning)
	}
	// These mutations happen after inference but before publication. They must
	// not rewrite either model's frozen forecast or weather inputs.
	legacy[0].PVW = -99999
	*weather[0].SolarWm2 = 999
	f.pv.SetRated(100000)
	f.load.SetHeatingCoef(900)
	in.Record(base, planning, "decision", at.Add(time.Second).UnixMilli())
	issue := (<-f.queue).issue
	if err := issue.Validate(); err != nil {
		t.Fatal(err)
	}
	champion, shadow := primarySeries(t, issue, "champion"), primarySeries(t, issue, "legacy_shadow")
	if champion.Points[0].PVW != 100 || shadow.Points[0].PVW != 2000 || *issue.Weather[0].GHIWm2 != 500 {
		t.Fatal("issued primary/shadow not frozen independently")
	}
	if champion.Points[0].LoadSource != "energyplan" || champion.Points[0].LoadQuality != "learning" || champion.Points[2].PVQuality != "learning" || shadow.Points[0].PVSource != "legacy" {
		t.Fatal("primary source or independently earned model quality lost")
	}
	if primarySeries(t, issue, "planning").Points[0].PVW != 50 || primarySeries(t, issue, "energyplan").Points[0].PVW != 100 {
		t.Fatal("risk adjustment overwrote issued model forecast")
	}
	st := hostForecastDB(t)
	if err := st.SaveForecastIssue(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
}

func TestForecastPrimarySelectsEachSignalAndRealZero(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	f := primaryFixture(at, func(_ context.Context, p []byte) ([]byte, error) {
		_, reply := primaryHostForecastReply(p)
		rows := reply["predictions"].([]any)
		rows[0].(map[string]any)["pv"] = map[string]any{"known": false, "quality": "unknown", "uncertainty": "unavailable", "coverage": 0}
		rows[1].(map[string]any)["pv"] = map[string]any{"known": true, "point_w": 0, "lower_w": 0, "upper_w": 50, "quality": "cold_start", "uncertainty": "provisional", "coverage": 0}
		rows[1].(map[string]any)["load"] = map[string]any{"known": false, "quality": "unknown", "uncertainty": "unavailable", "coverage": 0}
		return json.Marshal(reply)
	})
	in := f.Snapshot(at, trackerWeather(at, at))
	legacy := trackerSlots(at, 2)
	got := in.Resolve(context.Background(), legacy)
	if got[0].PVW != legacy[0].PVW || got[0].LoadW != 1100 || got[1].PVW != 0 || got[1].LoadW != legacy[1].LoadW {
		t.Fatalf("signal fallback or real zero wrong: %+v", got)
	}
	in.Record(got, got, "decision", at.UnixMilli())
	points := primarySeries(t, (<-f.queue).issue, "champion").Points
	if points[0].PVSource != "legacy" || points[0].ModelPV != nil || points[0].PVKnown || points[1].LoadSource != "legacy" || points[1].ModelLoad != nil || !points[1].PVKnown || points[1].PVQuality != "cold_start" {
		t.Fatalf("fallback fabricated knowledge or retained wrong model range: %+v", points)
	}
}

func TestForecastPrimaryUnavailableRetainsLegacy(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	for _, kind := range []string{"unsupported", "timeout", "invalid", "missing_quarter"} {
		t.Run(kind, func(t *testing.T) {
			f := primaryFixture(at, func(ctx context.Context, p []byte) ([]byte, error) {
				if kind == "timeout" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				_, reply := primaryHostForecastReply(p)
				rows := reply["predictions"].([]any)
				if kind == "invalid" {
					rows[0].(map[string]any)["pv"].(map[string]any)["point_w"] = -1
				}
				if kind == "missing_quarter" {
					reply["predictions"] = rows[:len(rows)-1]
				}
				return json.Marshal(reply)
			})
			if kind == "unsupported" {
				f.candidate = nil
			}
			in := f.Snapshot(at, trackerWeather(at, at))
			legacy := trackerSlots(at, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			got := in.Resolve(ctx, legacy)
			if !reflect.DeepEqual(got, legacy) {
				t.Fatalf("unavailable worker changed legacy fallback: %+v", got)
			}
			in.Record(got, got, "decision", at.UnixMilli())
			issue := (<-f.queue).issue
			if primarySeries(t, issue, "champion").Points[0].LoadSource != "legacy" {
				t.Fatal("fallback not explicit")
			}
		})
	}
}

func TestForecastPrimaryPartialCurrentIntervalUsesRustRemainder(t *testing.T) {
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	at := start.Add(7 * time.Minute)
	f := primaryFixture(at, primaryReply)
	in := f.Snapshot(at, trackerWeather(start, at))
	legacy := trackerSlots(start, 3)
	got := in.Resolve(context.Background(), legacy)
	if got[0].PVW != -100 || got[0].LoadW != 1100 || got[1].PVW != -200 || got[2].LoadW != 1300 {
		t.Fatalf("current interval did not use Rust remainder: %+v", got)
	}
	in.Record(got, got, "partial-decision", at.UnixMilli())
	issue := (<-f.queue).issue
	if err := issue.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"champion", "planning", "legacy_shadow", "energyplan"} {
		if primarySeries(t, issue, name).Points[0].PredictionStartMS != at.UnixMilli() {
			t.Fatalf("partial not explicit in %s", name)
		}
	}
}

func TestForecastPrimaryNativeColdLoadKeepsHeatingPriorUntilLearning(t *testing.T) {
	binary := os.Getenv("FTW_FORECAST_WORKER")
	if binary == "" {
		t.Skip("set FTW_FORECAST_WORKER for native primary integration")
	}
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	st := hostForecastDB(t)
	r, err := newRustForecast(st, binary)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f := trackerFixture(at)
	f.site, f.candidate = hostForecastSite, r
	f.load.SetHeatingCoef(300)
	weather := trackerWeather(at, at)
	weather[0].TempC = hostForecastPtr(0.0)
	in := f.Snapshot(at, weather)
	legacy := trackerSlots(at, 4)
	legacy[0].LoadW = in.Load(at)
	if legacy[0].LoadW < 5400 {
		t.Fatal("fixture lost the configured heating prior")
	}
	got := in.Resolve(context.Background(), legacy)
	if got[0].LoadW != legacy[0].LoadW {
		t.Fatalf("cold model replaced heating prior: %v -> %v", legacy[0].LoadW, got[0].LoadW)
	}
	in.Record(got, got, "native-decision", at.Add(time.Second).UnixMilli())
	issue := (<-f.queue).issue
	point := primarySeries(t, issue, "champion").Points[0]
	if point.LoadSource != "legacy" || !point.LoadKnown || point.ModelLoad != nil {
		t.Fatalf("cold fallback evidence missing: %+v", point)
	}
	if raw := primarySeries(t, issue, "energyplan").Points[0]; raw.LoadW != 500 || raw.LoadQuality != "cold_start" {
		t.Fatalf("raw cold forecast was not retained: %+v", raw)
	}
	trainNativeLoad(t, r, at)
	learned := f.Snapshot(at, weather)
	next := learned.Resolve(context.Background(), legacy)
	if next[0].LoadW >= 1000 {
		t.Fatalf("measured low load did not replace the prior: %+v", next[0])
	}
	learned.Record(next, next, "learned-decision", at.UnixMilli())
	if p := primarySeries(t, (<-f.queue).issue, "champion").Points[0]; p.LoadSource != "energyplan" || p.LoadQuality != "learning" {
		t.Fatalf("learned load did not take over: %+v", p)
	}
	if err := st.SaveForecastIssue(context.Background(), issue); err != nil {
		t.Fatal(err)
	}
}

func TestForecastPrimaryResolveUsesCapturedStateAndFeatures(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	var request energyforecast.PredictRequest
	f := primaryFixture(at, func(_ context.Context, p []byte) ([]byte, error) {
		if err := json.Unmarshal(p, &request); err != nil {
			t.Fatal(err)
		}
		return primaryReply(context.Background(), p)
	})
	r := f.candidate.(*rustForecast)
	r.saved = savedForecastState{SiteID: hostForecastSite().SiteID, ConfigRevision: hostForecastSite().Revision,
		LatestAvailableMS: at.Add(-time.Minute).UnixMilli(), State: json.RawMessage(`{"epoch":1}`)}
	away := false
	f.away = func(time.Time) bool { return away }
	weather := trackerWeather(at, at)
	in := f.Snapshot(at, weather)
	r.saved = savedForecastState{SiteID: hostForecastSite().SiteID, ConfigRevision: hostForecastSite().Revision,
		LatestAvailableMS: at.Add(time.Minute).UnixMilli(), State: json.RawMessage(`{"epoch":2}`)}
	away = true
	*weather[0].SolarWm2 = 999
	got := in.Resolve(context.Background(), trackerSlots(at, 1))
	if got[0].PVW != -100 || string(request.State) != `{"epoch":1}` || request.Horizon[0].Home == nil || !*request.Horizon[0].Home || *request.Horizon[0].GHIWm2 != 500 {
		t.Fatalf("live state or features leaked into primary request: %+v", request)
	}
	in.Record(got, got, "decision", at.Add(time.Second).UnixMilli())
	issue := (<-f.queue).issue
	for _, model := range issue.Models {
		if model.Name == "energyplan" && string(model.State) != `{"epoch":1}` {
			t.Fatal("archive stored post-origin model")
		}
	}
}

func TestForecastPrimaryNativeCurrentIntervalReplaysAfterNewObservation(t *testing.T) {
	binary := os.Getenv("FTW_FORECAST_WORKER")
	if binary == "" {
		t.Skip("set FTW_FORECAST_WORKER to a partial-capable native worker")
	}
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	origin := start.Add(7*time.Minute + 123*time.Millisecond)
	r, err := newRustForecast(hostForecastDB(t), binary)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f := trackerFixture(origin)
	f.site, f.candidate = hostForecastSite, r
	trainNativeLoad(t, r, start)
	capturedState := r.Snapshot()
	in := f.Snapshot(origin, trackerWeather(start, origin))
	legacy := trackerSlots(start, 4)
	got := in.Resolve(context.Background(), legacy)
	if got[0].LoadW == legacy[0].LoadW {
		t.Fatal("first current control interval still used legacy load")
	}
	in.Record(got, got, "native-partial", origin.Add(time.Second).UnixMilli())
	issue := (<-f.queue).issue
	primary := primarySeries(t, issue, "champion").Points[0]
	if primary.PredictionStartMS != origin.UnixMilli() || primary.LoadSource != "energyplan" {
		t.Fatalf("partial first interval provenance missing: %+v", primary)
	}
	obs := forecasting.Observation{StartMS: start.UnixMilli(), EndMS: start.Add(15 * time.Minute).UnixMilli(),
		AvailableAtMS: start.Add(15 * time.Minute).UnixMilli(), LoadW: 7500, LoadKnown: true}
	if err = r.Update(context.Background(), hostForecastSite(), obs, nil, false); err != nil {
		t.Fatal(err)
	}
	replay, err := r.Predict(context.Background(), hostForecastSite(), issue, capturedState, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replay.Series[0].Points, primarySeries(t, issue, "energyplan").Points) {
		// Bands are host calibration output rather than worker inference state.
		applyForecastBands(&replay, nil)
		if !reflect.DeepEqual(replay.Series[0].Points, primarySeries(t, issue, "energyplan").Points) {
			t.Fatal("new observation changed replayed remaining-interval forecast")
		}
	}
}

func trainNativeLoad(t *testing.T, r *rustForecast, target time.Time) {
	t.Helper()
	for _, days := range []int{-2, -1} {
		at := target.AddDate(0, 0, days)
		end := at.Add(15 * time.Minute)
		o := forecasting.Observation{StartMS: at.UnixMilli(), EndMS: end.UnixMilli(), AvailableAtMS: end.UnixMilli(), LoadW: 900, LoadKnown: true}
		if err := r.Update(context.Background(), hostForecastSite(), o, nil, false); err != nil {
			t.Fatal(err)
		}
	}
}
