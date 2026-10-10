package main

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/sunpos"
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

func forecastErrorsAtLead(history []forecasting.ErrorSample, ahead time.Duration) []forecasting.ErrorSample {
	for i := range history {
		e := &history[i]
		e.OriginMS = e.StartMS - ahead.Milliseconds()
		e.IssuedAtMS = e.OriginMS
		e.Lead = forecasting.LeadBucket(e.OriginMS, e.StartMS)
	}
	return history
}

func TestForecastSourceChoiceFollowsMeasuredErrors(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// These paired errors favor Energyplan load and legacy PV.
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
			if got.PV[1].Source != tc.pv || got.Load[1].Source != tc.load {
				t.Fatalf("got pv=%q load=%q, want pv=%q load=%q (%+v)", got.PV[1].Source, got.Load[1].Source, tc.pv, tc.load, got)
			}
		})
	}
	got := chooseForecastSources(pairedForecastErrors(t, "cfg", now, 72, measured), "cfg", now.UnixMilli())
	if got.Load[1].Hours != 72 || got.Load[1].EnergyplanMAEW != 100 || got.Load[1].LegacyMAEW != 2000 || got.PV[1].LegacyMAEW != 150 {
		t.Fatalf("evidence not reported: %+v", got)
	}
}

func TestForecastPVSourceChoiceKeepsHorizonsSeparate(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	near := forecastErrorsAtLead(pairedForecastErrors(t, "cfg", now, 72,
		forecastPair{energyplanPV: 1000, legacyPV: 1900, energyplanLoad: 1100, legacyLoad: 3000}), 30*time.Minute)
	far := forecastErrorsAtLead(pairedForecastErrors(t, "cfg", now, 72,
		forecastPair{energyplanPV: 1900, legacyPV: 1000, energyplanLoad: 1100, legacyLoad: 3000}), 18*time.Hour)
	history := append(near, far...)
	got := chooseForecastSources(history, "cfg", now.UnixMilli())
	if got.PV[0].Source != "legacy" || got.PV[4].Source != "energyplan" || got.Load[0].Source != "energyplan" || got.Load[4].Source != "energyplan" {
		t.Fatalf("horizon winners lost: %+v", got)
	}
	for _, lead := range []int{1, 2, 3, 5} {
		if got.PV[lead].Source != "" || got.PV[lead].Hours != 0 {
			t.Fatalf("lead %d borrowed another horizon's evidence: %+v", lead, got.PV[lead])
		}
	}
	// Several horizons cannot pool their few hours into a measured choice.
	thin := forecastErrorsAtLead(pairedForecastErrors(t, "cfg", now, 24,
		forecastPair{energyplanPV: 1000, legacyPV: 1900, energyplanLoad: 1100, legacyLoad: 3000}), 30*time.Minute)
	thin = append(thin, forecastErrorsAtLead(pairedForecastErrors(t, "cfg", now, 24,
		forecastPair{energyplanPV: 1000, legacyPV: 1900, energyplanLoad: 1100, legacyLoad: 3000}), 18*time.Hour)...)
	for _, pick := range chooseForecastSources(thin, "cfg", now.UnixMilli()).PV {
		if pick.Source != "" {
			t.Fatalf("thin horizons pooled their evidence: %+v", pick)
		}
	}
	// Future outcomes and nighttime zeros cannot supply daylight evidence.
	for i := range near {
		near[i].AvailableAtMS = now.Add(time.Hour).UnixMilli()
	}
	if pick := chooseForecastSources(near, "cfg", now.UnixMilli()).PV[0]; pick.Source != "" || pick.Samples != 0 {
		t.Fatalf("future truth chose a source: %+v", pick)
	}
	for i := range far {
		far[i].Daylight = false
	}
	if pick := chooseForecastSources(far, "cfg", now.UnixMilli()).PV[4]; pick.Source != "" || pick.Samples != 0 {
		t.Fatalf("nighttime truth chose a source: %+v", pick)
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
	f.errors = forecastErrorsAtLead(pairedForecastErrors(t, site.Revision, at, 72,
		forecastPair{energyplanPV: 1250, legacyPV: 1850, energyplanLoad: 1100, legacyLoad: 3000}), 30*time.Minute)
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
	if got[4].LoadW != legacy[4].LoadW || got[0].LoadW != 1100 || got[0].PVW != -100 {
		t.Fatalf("legacy load or default PV rule lost: %+v", got[0])
	}
}

func TestForecastPVHorizonChoiceReachesPlannerAndArchive(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	f := primaryFixture(at, primaryReply)
	site := hostForecastSite()
	site.HasPVScale = true
	f.site = func() forecastSite { return site }
	f.errors = forecastErrorsAtLead(pairedForecastErrors(t, site.Revision, at, 72,
		forecastPair{energyplanPV: 1000, legacyPV: 1900, energyplanLoad: 1100, legacyLoad: 3000}), 30*time.Minute)
	f.errors = append(f.errors, pairedForecastErrors(t, site.Revision, at, 72,
		forecastPair{energyplanPV: 1900, legacyPV: 1000, energyplanLoad: 1100, legacyLoad: 3000})...)
	later := at.Add(2 * time.Hour)
	weather := append(trackerWeather(at, at), trackerWeather(later, at)...)
	legacy := append(trackerSlots(at, 1), trackerSlots(later, 1)...)
	in := f.Snapshot(at, weather)
	resolved := in.Resolve(context.Background(), legacy)
	if resolved[0].PVW != -2000 || resolved[1].PVW != -100 {
		t.Fatalf("per-horizon PV did not reach MPC: %+v", resolved)
	}
	in.Record(resolved, resolved, "horizon-choice", at.UnixMilli())
	issue := (<-f.queue).issue
	champion := primarySeries(t, issue, "champion").Points
	if champion[0].PVSource != "legacy" || champion[0].ModelPV != nil || champion[1].PVSource != "energyplan" || champion[1].ModelPV == nil {
		t.Fatalf("selected provenance lost: %+v", champion)
	}
	for _, point := range primarySeries(t, issue, "legacy_shadow").Points {
		if point.PVW != 2000 || point.PVSource != "legacy" {
			t.Fatalf("source choice changed the frozen shadow: %+v", point)
		}
	}
}

func TestForecastLoadHorizonChoiceReachesPlannerAndArchive(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	f := primaryFixture(at, primaryReply)
	cohort := f.site().Revision
	f.errors = forecastErrorsAtLead(pairedForecastErrors(t, cohort, at, 72,
		forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: 3000, legacyLoad: 1100}), 30*time.Minute)
	f.errors = append(f.errors, pairedForecastErrors(t, cohort, at, 72,
		forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: 1100, legacyLoad: 3000})...)
	later := at.Add(2 * time.Hour)
	legacy := append(trackerSlots(at, 1), trackerSlots(later, 1)...)
	in := f.Snapshot(at, nil)
	resolved := in.Resolve(context.Background(), legacy)
	if resolved[0].LoadW != legacy[0].LoadW || resolved[1].LoadW != 1100 {
		t.Fatalf("per-horizon load did not reach MPC: %+v", resolved)
	}
	in.Record(resolved, resolved, "load-horizon-choice", at.UnixMilli())
	issue := (<-f.queue).issue
	champion := primarySeries(t, issue, "champion").Points
	if champion[0].LoadSource != "legacy" || champion[0].ModelLoad != nil || champion[1].LoadSource != "energyplan" || champion[1].ModelLoad == nil {
		t.Fatalf("selected load provenance lost: %+v", champion)
	}
	for _, point := range primarySeries(t, issue, "legacy_shadow").Points {
		if point.LoadW != legacy[0].LoadW || point.LoadSource != "legacy" {
			t.Fatalf("source choice changed the frozen shadow: %+v", point)
		}
	}
}

func TestForecastColdLoadCannotBorrowAnotherHorizonsEvidence(t *testing.T) {
	at := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	f := primaryFixture(at, func(_ context.Context, p []byte) ([]byte, error) {
		_, reply := hostForecastReply(p)
		return json.Marshal(reply)
	})
	f.errors = pairedForecastErrors(t, f.site().Revision, at, 72,
		forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: 1100, legacyLoad: 3000})
	legacy := append(trackerSlots(at, 1), trackerSlots(at.Add(2*time.Hour), 1)...)
	legacy[0].LoadW = 6000 // configured cold-weather household prior
	in := f.Snapshot(at, nil)
	got := in.Resolve(context.Background(), legacy)
	if got[0].LoadW != 6000 || got[1].LoadW != 1100 {
		t.Fatalf("cold load borrowed another horizon's evidence: %+v", got)
	}
	in.Record(got, got, "cold-load-horizon", at.UnixMilli())
	points := primarySeries(t, (<-f.queue).issue, "champion").Points
	if points[0].LoadSource != "legacy" || points[0].ModelLoad != nil || points[1].LoadSource != "energyplan" {
		t.Fatalf("cold load fallback provenance lost: %+v", points)
	}
}

func TestForecastLoadSourceChoiceRequiresQualifiedEvidence(t *testing.T) {
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func([]forecasting.ErrorSample)
	}{
		{"future truth", func(rows []forecasting.ErrorSample) {
			for i := range rows {
				rows[i].AvailableAtMS = at.Add(time.Hour).UnixMilli()
			}
		}},
		{"unknown load", func(rows []forecasting.ErrorSample) {
			for i := range rows {
				rows[i].LoadKnown = false
			}
		}},
		{"different outcomes", func(rows []forecasting.ErrorSample) {
			for i := range rows {
				if rows[i].Series == "legacy_shadow" {
					rows[i].LoadErrorW++
				}
			}
		}},
		{"different issues", func(rows []forecasting.ErrorSample) {
			for i := range rows {
				if rows[i].Series == "legacy_shadow" {
					rows[i].IssueID += "-other"
				}
			}
		}},
		{"invalid errors", func(rows []forecasting.ErrorSample) {
			for i := range rows {
				rows[i].LoadErrorW = math.NaN()
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := forecastErrorsAtLead(pairedForecastErrors(t, "cfg", at, 72,
				forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: 1100, legacyLoad: 3000}), 18*time.Hour)
			if pick := chooseForecastSources(rows, "cfg", at.UnixMilli()).Load[4]; pick.Source != "energyplan" {
				t.Fatalf("fixture lost its measured winner: %+v", pick)
			}
			tc.mutate(rows)
			for lead, pick := range chooseForecastSources(rows, "cfg", at.UnixMilli()).Load {
				if pick.Source != "" || pick.Samples != 0 {
					t.Fatalf("lead %d used unqualified load evidence: %+v", lead, pick)
				}
			}
		})
	}
}

func TestForecastDaytimeRiskAndArchivedBandsExcludeNightErrors(t *testing.T) {
	at := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	f := primaryFixture(at, primaryReply)
	// Both model sources miss PV by 1 kW during daylight. Nights have zero
	// error; sun position qualifies the historical quarters as in scoring.
	daylightHours := 0
	for day := 1; day <= 7; day++ {
		for hour := 0; hour < 24; hour++ {
			end := at.Add(-time.Duration(day*24) * time.Hour).Truncate(24 * time.Hour).Add(time.Duration(hour+1) * time.Hour)
			rows := pairedForecastErrors(t, f.site().Revision, end, 1,
				forecastPair{energyplanPV: 2000, legacyPV: 2000, energyplanLoad: 1000, legacyLoad: 1000})
			midpoint := time.UnixMilli(rows[0].StartMS + (rows[0].EndMS-rows[0].StartMS)/2)
			daylight := sunpos.At(midpoint, f.site().Latitude, f.site().Longitude).ZenithDeg < 90
			if daylight {
				daylightHours++
			}
			for i := range rows {
				rows[i].Daylight = daylight
				if rows[i].Daylight {
					rows[i].PVErrorW = -1000
				}
			}
			f.errors = append(f.errors, rows...)
		}
	}
	in := f.Snapshot(at, trackerWeather(at.Add(2*time.Hour), at))
	base := in.Resolve(context.Background(), trackerSlots(at.Add(2*time.Hour), 1))
	planning := append([]mpc.Slot(nil), base...)
	in.Risk(base, planning, 1)
	if planning[0].PVW != 0 || planning[0].LoadW != 2000 {
		t.Fatalf("night errors narrowed the daytime margin: base %+v planning %+v", base[0], planning[0])
	}
	in.Record(base, planning, "daytime-risk", at.UnixMilli())
	issue := (<-f.queue).issue
	point := primarySeries(t, issue, "energyplan").Points[0]
	if point.PVBand.Samples != daylightHours || point.NetBand.Samples != daylightHours || point.PVBand.Method != forecasting.BandMethodEmpirical || point.NetBand.HighW != 2000 {
		t.Fatalf("archive did not use daytime errors: %+v", point)
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
