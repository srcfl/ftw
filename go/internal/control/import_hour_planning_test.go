package control

import (
	"math"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/mpc"
)

// The planner gets the clock hour Core's live limit counts and what it has
// used. On 25 October 2026 02:00 comes twice in Stockholm, and each of the
// two hours counts its own import.
func TestPlanningImportCeilingOnTheLongDay(t *testing.T) {
	zone, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	var now time.Time
	st, store := newImportHourState(&now)
	st.PeakImportCeilingW, st.importHourZone = 4500, zone
	first := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC) // 02:00 CEST
	for _, hour := range []time.Time{first, first.Add(time.Hour)} {
		for at := hour; !at.After(hour.Add(20 * time.Minute)); at = at.Add(2 * time.Second) {
			feedMeter(st, store, &now, at, 3000)
		}
		got := PlanningImportCeiling(st)
		if !got.HourStart.Equal(hour) || got.HourStart.In(zone).Hour() != 2 || got.W != 4500 {
			t.Fatalf("at %s: hour from %s, ceiling %.0f W", now.In(zone).Format("15:04 MST"), got.HourStart.In(zone).Format("15:04 MST"), got.W)
		}
		if want := 3000 * 20 / 60.; math.Abs(got.ElapsedWh-want) > 1 {
			t.Fatalf("at %s: %.1f Wh used, want %.1f Wh", now.In(zone).Format("15:04 MST"), got.ElapsedWh, want)
		}
	}
	st.PeakImportCeilingW = 0
	if got := PlanningImportCeiling(st); got != (mpc.ImportCeiling{}) {
		t.Fatalf("ceiling off gave %+v", got)
	}
}

// A planned hour holds the ceiling less half a rise step, the margin
// TestSiteSimPlanUnderImportCeiling measures.
func TestPlanningImportCeilingKeepsHalfARiseStep(t *testing.T) {
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, hourTestZone)
	st, _ := newImportHourState(&now)
	st.PeakImportCeilingW = 4500
	// Half of 1 A on each of three 230 V phases.
	if got := PlanningImportCeiling(st); got.MeanW != 4155 {
		t.Fatalf("planned mean %.0f W, want 4155 W", got.MeanW)
	}
	st.PeakImportCeilingW = 300
	if got := PlanningImportCeiling(st); got.W != 300 || got.MeanW != 0 {
		t.Fatalf("a ceiling under half a step: %+v, want it on with no room", got)
	}
}

// Before a reading measures the hour, as after a restart or a new ceiling,
// the live limit lets the rest of the hour hold the ceiling: the time before
// counts as used. A restart that seeds the hour from state.db counts what
// the record covers, and the time it misses at the ceiling.
func TestPlanningImportCeilingAfterARestart(t *testing.T) {
	now := time.Date(2026, 10, 26, 9, 40, 0, 0, hourTestZone)
	st, store := newImportHourState(&now)
	st.PeakImportCeilingW = 4500
	if got := PlanningImportCeiling(st); math.Abs(got.ElapsedWh-3000) > 1e-6 || got.HourStart.Minute() != 0 {
		t.Fatalf("at boot: %.1f Wh used in the hour from %s; want 3000 Wh from 09:00", got.ElapsedWh, got.HourStart.Format("15:04"))
	}
	// state.db covered 30 of the 40 minutes with 1.2 kWh.
	st.SeedHourImport(now, 1200, 30*time.Minute)
	feedMeter(st, store, &now, now.Add(time.Second), 2000)
	if got := PlanningImportCeiling(st); math.Abs(got.ElapsedWh-(1200+4500/6.+2000/3600.)) > 1 {
		t.Fatalf("seeded: %.1f Wh used, want 1200 Wh plus 10 unseen minutes at the ceiling", got.ElapsedWh)
	}
	now = now.Add(10 * time.Minute)
	feedMeter(st, store, &now, now, 2000)
	st.PeakImportCeilingW = 3000 // set during the hour, not yet seen by a reading
	if got := PlanningImportCeiling(st); math.Abs(got.ElapsedWh-2500) > 1 {
		t.Fatalf("new ceiling: %.1f Wh used, want 50 minutes at 3 kW", got.ElapsedWh)
	}
}

// The demand charges' hour in progress counts what the site meter measured,
// with or without an import ceiling, and only the time its readings cover:
// the planner fills the rest itself.
func TestMeasuredHourImportCountsTheMeteredHour(t *testing.T) {
	var now time.Time
	st, store := newImportHourState(&now)
	st.PeakImportCeilingW = 0
	hour := time.Date(2026, 9, 9, 17, 0, 0, 0, hourTestZone)
	if got := MeasuredHourImport(st); got != (mpc.HourImport{}) {
		t.Fatalf("before a reading %+v", got)
	}
	for at := hour.Add(5 * time.Minute); !at.After(hour.Add(20 * time.Minute)); at = at.Add(2 * time.Second) {
		feedMeter(st, store, &now, at, 3000)
	}
	got := MeasuredHourImport(st)
	if !got.Start.Equal(hour) || got.Covered != 15*time.Minute || math.Abs(got.ImportedWh-3000*15/60.) > 1 {
		t.Fatalf("got %+v; want 750 Wh over the 15 minutes from 17:05", got)
	}
	// After a restart the record restores the hour; what it misses is not covered.
	seeded, _ := newImportHourState(&now)
	seeded.SeedHourImport(now, 600, 12*time.Minute)
	if got := MeasuredHourImport(seeded); !got.Start.Equal(hour) || got.Covered != 12*time.Minute || got.ImportedWh != 600 {
		t.Fatalf("seeded %+v", got)
	}
	// In the next hour, before its first reading, the last hour's import is gone.
	now = hour.Add(time.Hour + time.Minute)
	if got := MeasuredHourImport(st); got != (mpc.HourImport{}) {
		t.Fatalf("next hour %+v", got)
	}
}
