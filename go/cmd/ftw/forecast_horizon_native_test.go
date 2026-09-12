package main

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestForecastPrimaryNativeFullHorizonArchivesEveryInterval(t *testing.T) {
	binary := os.Getenv("FTW_FORECAST_WORKER")
	if binary == "" {
		t.Skip("set FTW_FORECAST_WORKER for native primary integration")
	}
	for _, tc := range []struct {
		name   string
		offset time.Duration
		count  int
	}{
		{"quarter_boundary", 0, 192},
		{"partial_quarter", 7*time.Minute + 123*time.Millisecond, 193},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now().UTC().Truncate(15 * time.Minute).Add(-time.Hour)
			origin := start.Add(tc.offset)
			until := origin.Add(48 * time.Hour)
			st := hostForecastDB(t)
			worker, err := newRustForecast(st, binary)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { worker.Close() })
			// Train PV through the worker while load still uses its cold prior.
			// Unlearned parts of the PV curve must keep signal-specific fallback.
			observed := start.Truncate(24 * time.Hour).Add(-12 * time.Hour)
			observation := forecasting.Observation{StartMS: observed.UnixMilli(), EndMS: observed.Add(15 * time.Minute).UnixMilli(),
				AvailableAtMS: observed.Add(15 * time.Minute).UnixMilli(), PVW: 2300, PVKnown: true}
			observedWeather := trackerWeather(observed, observed)[0]
			if err := worker.Update(context.Background(), hostForecastSite(), observation, &observedWeather, false); err != nil {
				t.Fatalf("train native PV model: %v", err)
			}
			tracker := trackerFixture(origin)
			tracker.site, tracker.candidate = hostForecastSite, worker
			var weather []state.ForecastPoint
			for at := start.Truncate(time.Hour); at.Before(until); at = at.Add(time.Hour) {
				weather = append(weather, trackerWeather(at, origin)...)
			}
			var legacy []mpc.Slot
			for at := start; at.Before(until); at = at.Add(15 * time.Minute) {
				legacy = append(legacy, mpc.Slot{StartMs: at.UnixMilli(), LenMin: 15, PVW: -2000, LoadW: 98765})
			}
			if len(legacy) != tc.count {
				t.Fatalf("48-hour fixture has %d intervals, want %d", len(legacy), tc.count)
			}
			inputs := tracker.Snapshot(origin, weather)
			resolved := inputs.Resolve(context.Background(), legacy)
			if len(resolved) != tc.count {
				t.Fatalf("primary changed horizon length: %d", len(resolved))
			}
			for i, slot := range resolved {
				if slot.LoadW != legacy[i].LoadW {
					t.Fatalf("cold load replaced site prior at interval %d of %d", i, tc.count)
				}
			}
			inputs.Record(resolved, resolved, "full-horizon-"+tc.name, origin.Add(time.Second).UnixMilli())
			issue := (<-tracker.queue).issue
			if err := issue.Validate(); err != nil {
				t.Fatalf("composed forecast rejected: %v", err)
			}
			for _, series := range issue.Series {
				if len(series.Points) != tc.count {
					t.Fatalf("%s lost intervals: %d, want %d", series.Name, len(series.Points), tc.count)
				}
				last := series.Points[tc.count-1]
				if last.StartMS != legacy[tc.count-1].StartMs || last.EndMS != legacy[tc.count-1].StartMs+15*time.Minute.Milliseconds() {
					t.Fatalf("%s changed the final interval: %+v", series.Name, last)
				}
			}
			var partialStart int64
			if tc.offset > 0 {
				partialStart = origin.UnixMilli()
			}
			for _, name := range []string{"champion", "planning", "legacy_shadow", "energyplan"} {
				first := primarySeries(t, issue, name).Points[0]
				if first.StartMS != start.UnixMilli() || first.PredictionStartMS != partialStart {
					t.Fatalf("%s lost current-interval provenance: %+v", name, first)
				}
			}
			native := primarySeries(t, issue, "energyplan").Points
			positiveNativePV := 0
			for i, point := range primarySeries(t, issue, "champion").Points {
				if point.LoadSource != "legacy" || native[i].LoadQuality != "cold_start" || native[i].LoadW != 500 || point.LoadW != resolved[i].LoadW || point.PVW != -resolved[i].PVW {
					t.Fatalf("primary source or value changed at interval %d: %+v", i, point)
				}
				if native[i].PVKnown {
					if point.PVSource != "energyplan" || point.PVW != native[i].PVW {
						t.Fatalf("known native PV fell back at interval %d: %+v", i, point)
					}
					if point.PVW > 0 {
						positiveNativePV++
					}
				} else if point.PVSource != "legacy" || resolved[i].PVW != legacy[i].PVW {
					t.Fatalf("unknown native PV replaced legacy at interval %d", i)
				}
			}
			if positiveNativePV == 0 {
				t.Fatal("native PV was never selected for positive generation")
			}
			if err := st.SaveForecastIssue(context.Background(), issue); err != nil {
				t.Fatalf("archive rejected full composed forecast: %v", err)
			}
			restored, err := st.LoadForecastIssues(context.Background(), origin.Add(-time.Minute).UnixMilli(), origin.Add(time.Minute).UnixMilli(), 1)
			if err != nil || len(restored) != 1 {
				t.Fatalf("archive reload: issues=%d err=%v", len(restored), err)
			}
			if restored[0].ID != issue.ID || !reflect.DeepEqual(restored[0].Series, issue.Series) {
				t.Fatal("archive changed issued series, sources, or interval bounds")
			}
			if !reflect.DeepEqual(restored[0].Weather, issue.Weather) || !reflect.DeepEqual(restored[0].Occupancy, issue.Occupancy) || !reflect.DeepEqual(restored[0].Site, issue.Site) {
				t.Fatal("archive changed frozen forecast inputs")
			}
			if len(restored[0].Models) != len(issue.Models) {
				t.Fatal("archive lost model references")
			}
			for i, model := range restored[0].Models {
				if model.StateID == "" || len(model.State) != 0 {
					t.Fatalf("%s has no archived state reference", model.Name)
				}
				payload, err := st.LoadForecastModelState(context.Background(), model.StateID)
				if err != nil || !bytes.Equal(payload, issue.Models[i].State) {
					t.Fatalf("%s model payload changed: %v", model.Name, err)
				}
				model.State, model.StateID = payload, ""
				if !reflect.DeepEqual(model, issue.Models[i]) {
					t.Fatalf("%s model identity or quality changed", model.Name)
				}
			}
		})
	}
}
