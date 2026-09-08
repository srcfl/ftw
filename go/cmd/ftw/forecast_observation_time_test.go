package main

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/forecasting"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestForecastObservationUsesCurrentTelemetryCutoff(t *testing.T) {
	ctx := context.Background()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.InitForecastArchive(ctx); err != nil {
		t.Fatal(err)
	}

	tel := telemetry.NewStore()
	site := trackerSite()
	f := trackerFixture(time.Now())
	f.store = st
	f.tele = tel
	f.site = func() forecastSite { return site }
	var observedAt time.Time
	f.curtailed = func(at time.Time) bool {
		observedAt = at
		return false
	}
	reading := func(at time.Time) telemetry.ForecastReading {
		return telemetry.ForecastReading{
			At: at, Earliest: at, Latest: at, PVEarliest: at, PVLatest: at,
			Valid: true, PVValid: true, HouseholdW: 1200, PVW: -800,
		}
	}
	feed := func(at time.Time) []forecasting.Observation {
		return f.observationIntervals(reading(at), site, at)
	}

	// Prime the real accumulator from the start of the current quarter through
	// the scheduled tick. A synchronous archive retry can delay observe until a
	// newer telemetry sample has arrived.
	scheduledAt := time.Now().UTC()
	quarterStart := scheduledAt.Truncate(15 * time.Minute)
	if out := feed(quarterStart); len(out) != 0 {
		t.Fatalf("first sample completed an interval: %+v", out)
	}
	for at := quarterStart.Add(time.Minute); at.Before(scheduledAt); at = at.Add(time.Minute) {
		if out := feed(at); len(out) != 0 {
			t.Fatalf("partial quarter completed early: %+v", out)
		}
	}
	if scheduledAt.After(quarterStart) {
		if out := feed(scheduledAt); len(out) != 0 {
			t.Fatalf("scheduled sample completed early: %+v", out)
		}
	}

	tel.Update("site", telemetry.DerMeter, 400, nil, nil)
	tel.Update("pv", telemetry.DerPV, -800, nil, nil)
	tel.RecordDriverSuccess("site")
	tel.RecordDriverSuccess("pv")
	f.observe(ctx)
	if observedAt.IsZero() {
		t.Fatal("observation did not apply the curtailment cutoff")
	}

	var completed []forecasting.Observation
	quarterEnd := quarterStart.Add(15 * time.Minute)
	if observedAt.Before(quarterEnd) {
		for at := observedAt.Add(time.Minute); at.Before(quarterEnd); at = at.Add(time.Minute) {
			completed = append(completed, feed(at)...)
		}
		completed = append(completed, feed(quarterEnd)...)
	}
	persisted, err := st.LoadForecastObservations(ctx, quarterStart.UnixMilli(), quarterEnd.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	completed = append(completed, persisted...)
	if len(completed) != 1 || !completed[0].LoadKnown || completed[0].Quality != telemetry.ForecastIntervalQuality ||
		!completed[0].PVKnown || math.Abs(completed[0].LoadW-1200) > 1e-6 || math.Abs(completed[0].PVW-800) > 1e-6 ||
		completed[0].StartMS != quarterStart.UnixMilli() || completed[0].EndMS != quarterEnd.UnixMilli() {
		t.Fatalf("delayed observation lost or changed the complete quarter: %+v", completed)
	}
}

func TestForecastObservationStillRejectsLongGap(t *testing.T) {
	f := &forecastTracker{}
	site := trackerSite()
	start := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	reading := func(at time.Time) telemetry.ForecastReading {
		return telemetry.ForecastReading{At: at, Earliest: at, Latest: at, Valid: true, HouseholdW: 1200}
	}
	if out := f.observationIntervals(reading(start), site, start); len(out) != 0 {
		t.Fatalf("first sample completed an interval: %+v", out)
	}
	if out := f.observationIntervals(reading(start.Add(3*time.Minute)), site, start.Add(3*time.Minute)); len(out) != 0 {
		t.Fatalf("long gap produced an interval: %+v", out)
	}
	var completed []forecasting.Observation
	for at := start.Add(4 * time.Minute); !at.After(start.Add(15 * time.Minute)); at = at.Add(time.Minute) {
		completed = append(completed, f.observationIntervals(reading(at), site, at)...)
	}
	if len(completed) != 0 {
		t.Fatalf("quarter with a long gap was accepted: %+v", completed)
	}
}
