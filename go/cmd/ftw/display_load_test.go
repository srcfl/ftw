package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

// Two series: the live display load is logged as a Core metric exactly as
// clients show it (negative included). The history tick's Load stays the
// placeholder the source-energy writer replaces, so training, which reads
// HouseholdIntervals, never sees the display value.
func TestDisplayLoadIsLoggedApartFromTrainingLoad(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.EnableHistoryAggregation(); err != nil {
		t.Fatal(err)
	}
	tel := telemetry.NewStore()
	for _, d := range []string{"meter", "battery"} {
		tel.EnsureDriverHealth(d)
	}
	tel.Update("meter", telemetry.DerMeter, 1200, nil, nil)
	tel.Update("battery", telemetry.DerBattery, 2000, nil, nil) // negative balance
	tel.RecordDriverSuccess("meter")
	tel.RecordDriverSuccess("battery")
	ctrl := tickPersistControl{SiteMeterDriver: "meter"}
	at := time.Now().UnixMilli()
	if _, err := persistTelemetryTick(st, tel, ctrl, at, time.Minute, testEnergyIdentity(st)); err != nil {
		t.Fatal(err)
	}
	if err := st.FlushHistory(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []state.Sample
	for i := 0; i < 50 && len(got) == 0; i++ {
		got, err = st.LoadSeries(displayLoadDriver, displayLoadMetric, at-1, at+1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if len(got) != 1 || got[0].Value != -800 {
		t.Fatalf("display load series = %+v, want one -800 W sample", got)
	}
	history, err := st.LoadHistory(at-1, at+1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range history {
		if h.Missing&state.MissingLoad == 0 || h.LoadW != 0 {
			t.Fatalf("display load leaked into the history Load column: %+v", h)
		}
	}
	if intervals, _, err := st.HouseholdIntervals(context.Background(), 0, at, 15*60000); err != nil || len(intervals) != 0 {
		t.Fatalf("training intervals from one display tick: %v %v", intervals, err)
	}
}

func TestDisplayLoadNotLoggedWithoutSiteMeter(t *testing.T) {
	tel := telemetry.NewStore()
	if _, ok := displayLoadSample(tel, "meter", time.Now().UnixMilli(), telemetry.ForecastOptions{}, historySun{}); ok {
		t.Fatal("display load logged without a site meter")
	}
}
