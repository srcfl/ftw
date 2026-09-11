package energyforecast_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestNativeForecastResetIsSignalScopedAndDurable(t *testing.T) {
	binary := os.Getenv("FTW_FORECAST_WORKER")
	if binary == "" {
		t.Skip("set FTW_FORECAST_WORKER to an Energyplan worker with forecast reset support")
	}
	newClient := func() (*energyforecast.Client, *mpc.ProcessTransport) {
		transport, err := mpc.NewProcessTransport(mpc.ProcessTransportConfig{Command: []string{binary}, ModuleDir: filepath.Dir(binary)})
		if err != nil {
			t.Fatal(err)
		}
		return energyforecast.NewClient(transport), transport
	}
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	quarter := 15 * time.Minute
	end := start.Add(quarter)
	features := func(at time.Time) energyforecast.Features {
		ghi := 500.0
		available := start.UnixMilli()
		return energyforecast.Features{
			LocalDay: at.Unix() / 86400, LocalWeekday: (int(at.Weekday()) + 6) % 7,
			LocalMinute: at.Hour()*60 + at.Minute(), GHIWm2: &ghi, WeatherAvailableAtMs: &available,
		}
	}
	config := energyforecast.Config{PV: &energyforecast.PVConfig{LatitudeDeg: 57, LongitudeDeg: 15}, Load: &energyforecast.LoadConfig{}}
	observation := func(at time.Time, pv, load float64) energyforecast.Observation {
		return energyforecast.Observation{
			Interval: energyforecast.Interval{ValidStartMs: at.UnixMilli(), ValidEndMs: at.Add(quarter).UnixMilli()},
			Features: features(at), AvailableAtMs: at.Add(quarter).UnixMilli(),
			PVAvailableW: &pv, HouseholdLoadW: &load, PVQuality: energyforecast.QualityGood, LoadQuality: energyforecast.QualityGood,
		}
	}
	for _, selected := range []string{"pv", "load"} {
		t.Run(selected, func(t *testing.T) {
			client, transport := newClient()
			meta := energyforecast.RequestContext{RequestID: "native-reset-train-" + selected, SiteID: "native-reset-site", ConfigRevision: "v1", OriginMs: end.UnixMilli(), Config: config}
			pv, load := 2300.0, 470.0
			trained, err := client.Update(context.Background(), energyforecast.UpdateRequest{RequestContext: meta, Observations: []energyforecast.Observation{observation(start, pv, load)}})
			if err != nil {
				t.Fatal(err)
			}
			if trained.LatestTrainingMs.PV == nil || trained.LatestTrainingMs.Load == nil {
				t.Fatalf("training clocks = %#v", trained.LatestTrainingMs)
			}
			cutoff := end.UnixMilli()
			meta.RequestID = "native-reset-" + selected
			meta.OriginMs = cutoff
			meta.State = trained.State
			reset, err := client.Reset(context.Background(), energyforecast.ResetRequest{RequestContext: meta, Signal: selected, LearningStartedMs: cutoff})
			if err != nil {
				t.Fatal(err)
			}
			if reset.Signal != selected || reset.LearningStartedMs != cutoff || len(reset.State) == 0 {
				t.Fatalf("reset response = %#v", reset)
			}
			if reset.ModelRevision != trained.ModelRevision+1 {
				t.Fatalf("reset revision = %d, want %d", reset.ModelRevision, trained.ModelRevision+1)
			}
			if selected == "pv" {
				if reset.LatestTrainingMs.PV != nil || reset.LatestTrainingMs.Load == nil {
					t.Fatalf("PV reset clocks = %#v", reset.LatestTrainingMs)
				}
			} else if reset.LatestTrainingMs.Load != nil || reset.LatestTrainingMs.PV == nil {
				t.Fatalf("load reset clocks = %#v", reset.LatestTrainingMs)
			}
			other := "pv"
			if selected == "pv" {
				other = "load"
			}
			meta.RequestID = "native-reset-other-" + other
			meta.State = reset.State
			both, err := client.Reset(context.Background(), energyforecast.ResetRequest{RequestContext: meta, Signal: other, LearningStartedMs: cutoff})
			if err != nil {
				t.Fatal(err)
			}
			if both.LatestTrainingMs.PV != nil || both.LatestTrainingMs.Load != nil {
				t.Fatalf("both reset clocks = %#v", both.LatestTrainingMs)
			}

			// State is the saved worker snapshot. A fresh worker must retain the
			// two independently applied cutoffs before it receives old history.
			if err := transport.Close(); err != nil {
				t.Fatal(err)
			}
			client, transport = newClient()
			t.Cleanup(func() { _ = transport.Close() })
			meta.RequestID = "native-reset-old-input-" + selected
			meta.OriginMs = cutoff + quarter.Milliseconds()
			meta.State = both.State
			oldPV, oldLoad := 9000.0, 9000.0
			afterOld, err := client.Update(context.Background(), energyforecast.UpdateRequest{RequestContext: meta, Observations: []energyforecast.Observation{observation(start, oldPV, oldLoad)}})
			if err != nil {
				t.Fatal(err)
			}
			selectedCounts, otherCounts := afterOld.Updates.PV, afterOld.Updates.Load
			selectedClock, otherClock := afterOld.LatestTrainingMs.PV, afterOld.LatestTrainingMs.Load
			if selected == "load" {
				selectedCounts, otherCounts = afterOld.Updates.Load, afterOld.Updates.PV
				selectedClock, otherClock = afterOld.LatestTrainingMs.Load, afterOld.LatestTrainingMs.PV
			}
			if selectedCounts.Applied != 0 || selectedCounts.Skipped != 1 || selectedClock != nil {
				t.Fatalf("old %s history resurrected: counts=%#v clock=%v", selected, selectedCounts, selectedClock)
			}
			if otherCounts.Applied != 0 || otherCounts.Skipped != 1 || otherClock != nil {
				t.Fatalf("old %s history resurrected: counts=%#v clock=%v", other, otherCounts, otherClock)
			}
		})
	}
}
