package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/mpc"
)

type forecastPair struct{ energyplanPV, legacyPV, energyplanLoad, legacyLoad float64 }

// scoredForecastErrors scores one series in every hour before end against
// 2,000 W PV and 1,000 W load, from issues made two hours ahead.
func scoredForecastErrors(t *testing.T, series, cohort string, end time.Time, hours int, pvW, loadW float64, pvSource, loadSource string) []forecasting.ErrorSample {
	t.Helper()
	band := forecasting.Band{LowW: 0, HighW: 5000, Method: forecasting.BandMethodColdStart}
	var out []forecasting.ErrorSample
	for h := 1; h <= hours; h++ {
		at := end.Add(-time.Duration(h) * time.Hour)
		issued := at.Add(-2 * time.Hour)
		e := forecasting.ErrorSample{Series: series, ConfigVersion: cohort, IssueID: "issue-" + issued.Format(time.RFC3339),
			OriginMS: issued.UnixMilli(), IssuedAtMS: issued.UnixMilli(), StartMS: at.UnixMilli(), EndMS: at.Add(15 * time.Minute).UnixMilli(),
			AvailableAtMS: at.Add(15 * time.Minute).UnixMilli(), Lead: forecasting.LeadBucket(issued.UnixMilli(), at.UnixMilli()),
			PVErrorW: 2000 - pvW, LoadErrorW: 1000 - loadW, PVKnown: true, Daylight: true, LoadKnown: true,
			Prediction: forecasting.Point{StartMS: at.UnixMilli(), EndMS: at.Add(15 * time.Minute).UnixMilli(), PVW: pvW, LoadW: loadW,
				PVKnown: true, LoadKnown: true, PVQuality: "test", LoadQuality: "test", PVSource: pvSource, LoadSource: loadSource,
				PVBand: band, LoadBand: band, NetBand: band}}
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// pairedForecastErrors scores both sources of the same issues.
func pairedForecastErrors(t *testing.T, cohort string, end time.Time, hours int, p forecastPair) []forecasting.ErrorSample {
	t.Helper()
	return append(scoredForecastErrors(t, "energyplan", cohort, end, hours, p.energyplanPV, p.energyplanLoad, "energyplan", "energyplan"),
		scoredForecastErrors(t, "legacy_shadow", cohort, end, hours, p.legacyPV, p.legacyLoad, "legacy", "legacy")...)
}

func TestForecastSourceChoiceFollowsMeasuredErrors(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// Home box, 22 Sep–1 Oct: Energyplan load and legacy PV were clearly better.
	measured := forecastPair{energyplanPV: 1250, legacyPV: 1850, energyplanLoad: 1100, legacyLoad: 3000}
	for _, tc := range []struct {
		name     string
		history  []forecasting.ErrorSample
		pv, load string
	}{
		{"enough evidence", pairedForecastErrors(t, "cfg", now, 72, measured), "legacy", "energyplan"},
		{"one day is not enough", pairedForecastErrors(t, "cfg", now, 24, measured), "", ""},
		{"small gap keeps the quality rule", pairedForecastErrors(t, "cfg", now, 72,
			forecastPair{energyplanPV: 1950, legacyPV: 1952, energyplanLoad: 1100, legacyLoad: 1105}), "", ""},
		{"another cohort", pairedForecastErrors(t, "old", now, 72, measured), "", ""},
		{"older than a week", pairedForecastErrors(t, "cfg", now.Add(-8*24*time.Hour), 72, measured), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseForecastSources(tc.history, "cfg", now.UnixMilli())
			if got.PV.Source != tc.pv || got.Load.Source != tc.load {
				t.Fatalf("got pv=%q load=%q, want pv=%q load=%q (%+v)", got.PV.Source, got.Load.Source, tc.pv, tc.load, got)
			}
		})
	}
	got := chooseForecastSources(pairedForecastErrors(t, "cfg", now, 72, measured), "cfg", now.UnixMilli())
	if got.Load.Hours != 72 || got.Load.EnergyplanMAEW != 100 || got.Load.LegacyMAEW != 2000 || got.PV.LegacyMAEW != 150 {
		t.Fatalf("evidence not reported: %+v", got)
	}
}

func TestForecastSourceChoiceSteersResolve(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// The worker says load is cold and PV ready; measured errors say otherwise.
	f := primaryFixture(at, func(_ context.Context, p []byte) ([]byte, error) {
		_, reply := hostForecastReply(p)
		return json.Marshal(reply)
	})
	f.errors = pairedForecastErrors(t, f.site().Revision, at, 72,
		forecastPair{energyplanPV: 1250, legacyPV: 1850, energyplanLoad: 1100, legacyLoad: 3000})
	in := f.Snapshot(at, trackerWeather(at, at))
	legacy := trackerSlots(at, 2)
	got := in.Resolve(context.Background(), legacy)
	if got[0].PVW != legacy[0].PVW || got[0].LoadW != 1100 {
		t.Fatalf("measured errors did not choose the sources: %+v", got)
	}
	in.Record(got, got, "decision", at.UnixMilli())
	point := primarySeries(t, (<-f.queue).issue, "champion").Points[0]
	if point.PVSource != "legacy" || point.LoadSource != "energyplan" || point.LoadQuality != "cold_start" || point.ModelLoad == nil {
		t.Fatalf("chosen sources not recorded: %+v", point)
	}

	// Learned Energyplan load still yields to a clearly better legacy load.
	f = primaryFixture(at, primaryReply)
	f.errors = pairedForecastErrors(t, f.site().Revision, at, 72,
		forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: 3000, legacyLoad: 1100})
	in = f.Snapshot(at, trackerWeather(at, at))
	got = in.Resolve(context.Background(), legacy)
	if got[0].LoadW != legacy[0].LoadW || got[0].PVW != -100 {
		t.Fatalf("legacy load or default PV rule lost: %+v", got)
	}
}

func TestForecastSourceChoiceCalibratesOnlyTheChosenSources(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	f := primaryFixture(at, primaryReply)
	cohort := f.site().Revision
	// Legacy load measured far better, so it now feeds the plan. The champion
	// errors come from before, when Energyplan load overshot by 2 kW.
	f.errors = append(pairedForecastErrors(t, cohort, at, 8*24, forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: 3000, legacyLoad: 1100}),
		scoredForecastErrors(t, "champion", cohort, at, 8*24, 2000, 3000, "energyplan", "energyplan")...)
	in := f.Snapshot(at, nil)
	base := trackerSlots(at.Add(2*time.Hour), 1)
	base[0].PVW, base[0].LoadW = 0, 1000
	base = in.Resolve(context.Background(), base)
	if base[0].LoadW != 1000 {
		t.Fatalf("legacy load not chosen: %+v", base[0])
	}
	planning := append([]mpc.Slot(nil), base...)
	in.Risk(base, planning, 1)
	// A band from the old errors would sit 2 kW below the forecast and add
	// nothing. The cold band for a 1 kW load at lead 1 adds 400 W.
	if planning[0].LoadW != 1400 {
		t.Fatalf("band used errors of a source the plan no longer uses: %+v", planning[0])
	}
}
