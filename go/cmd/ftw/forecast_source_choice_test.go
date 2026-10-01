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
	site := hostForecastSite()
	site.HasPVScale = true // legacy PV is known where it has weather
	f.site = func() forecastSite { return site }
	f.errors = pairedForecastErrors(t, site.Revision, at, 72,
		forecastPair{energyplanPV: 1250, legacyPV: 1850, energyplanLoad: 1100, legacyLoad: 3000})
	in := f.Snapshot(at, trackerWeather(at, at))
	legacy := trackerSlots(at, 5) // weather covers the first hour only
	got := in.Resolve(context.Background(), legacy)
	if got[0].PVW != legacy[0].PVW || got[0].LoadW != 1100 {
		t.Fatalf("measured errors did not choose the sources: %+v", got[0])
	}
	if got[4].PVW != -100 {
		t.Fatalf("legacy PV without weather replaced Energyplan PV: %+v", got[4])
	}
	in.Record(got, got, "decision", at.UnixMilli())
	points := primarySeries(t, (<-f.queue).issue, "champion").Points
	if p := points[0]; p.PVSource != "legacy" || p.LoadSource != "energyplan" || p.LoadQuality != "cold_start" || p.ModelLoad == nil {
		t.Fatalf("chosen sources not recorded: %+v", p)
	}
	if points[4].PVSource != "energyplan" {
		t.Fatalf("fallback to Energyplan PV not recorded: %+v", points[4])
	}

	// Learned Energyplan load still yields to a clearly better legacy load.
	f = primaryFixture(at, primaryReply)
	f.errors = pairedForecastErrors(t, f.site().Revision, at, 72,
		forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: 3000, legacyLoad: 1100})
	in = f.Snapshot(at, trackerWeather(at, at))
	got = in.Resolve(context.Background(), legacy)
	if got[0].LoadW != legacy[0].LoadW || got[0].PVW != -100 {
		t.Fatalf("legacy load or default PV rule lost: %+v", got[0])
	}
}

// Each slot's margin comes from the errors its own sources made, not from
// champion errors of sources the plan used before.
func TestForecastRiskUsesTheSlotsOwnSources(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		energyplanLoad float64 // predicted against a 1,000 W actual
		legacyLoad     float64
		championSource string
		championLoad   float64
		wantMargin     float64
	}{
		// Both loads miss by 1 kW, so the quality rule picks learned
		// Energyplan load. Its errors set the margin, not legacy's.
		{"quality rule after legacy", 0, 2000, "legacy", 2000, 1000},
		// Legacy load wins by measurement. The margin uses its 300 W
		// shortfall with Energyplan PV, not Energyplan's old overshoot.
		{"measured legacy load", 3000, 700, "energyplan", 3000, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := primaryFixture(at, primaryReply)
			cohort := f.site().Revision
			f.errors = append(pairedForecastErrors(t, cohort, at, 8*24,
				forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: tc.energyplanLoad, legacyLoad: tc.legacyLoad}),
				scoredForecastErrors(t, "champion", cohort, at, 8*24, 2000, tc.championLoad, tc.championSource, tc.championSource)...)
			in := f.Snapshot(at, nil)
			base := trackerSlots(at.Add(2*time.Hour), 1)
			base[0].PVW, base[0].LoadW = 0, 1000
			base = in.Resolve(context.Background(), base)
			planning := append([]mpc.Slot(nil), base...)
			in.Risk(base, planning, 1)
			if got := (planning[0].LoadW + planning[0].PVW) - (base[0].LoadW + base[0].PVW); got != tc.wantMargin {
				t.Fatalf("margin %v W, want %v W: base %+v, planning %+v", got, tc.wantMargin, base[0], planning[0])
			}
		})
	}
}
