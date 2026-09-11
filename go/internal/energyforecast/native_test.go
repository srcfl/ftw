package energyforecast_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/energyforecast"
	"github.com/srcfl/ftw/go/internal/mpc"
)

func TestNativeForecastStateReplay(t *testing.T) {
	binary := os.Getenv("FTW_FORECAST_WORKER")
	if binary == "" {
		t.Skip("set FTW_FORECAST_WORKER to an Energyplan worker with forecast protocol v1")
	}
	newClient := func() (*energyforecast.Client, *mpc.ProcessTransport) {
		transport, err := mpc.NewProcessTransport(mpc.ProcessTransportConfig{Command: []string{binary}, ModuleDir: filepath.Dir(binary)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transport.Close() })
		return energyforecast.NewClient(transport), transport
	}
	c, transport := newClient()
	start := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	end := start.Add(15 * time.Minute)
	features := func(at time.Time) energyforecast.Features {
		ghi := 500.0
		available := start.UnixMilli()
		return energyforecast.Features{LocalDay: at.Unix() / 86400, LocalWeekday: (int(at.Weekday()) + 6) % 7, LocalMinute: at.Hour()*60 + at.Minute(), GHIWm2: &ghi, WeatherAvailableAtMs: &available}
	}
	meta := energyforecast.RequestContext{RequestID: "native-pv-update", SiteID: "native-geometry-free", ConfigRevision: "v1", OriginMs: end.UnixMilli(), Config: energyforecast.Config{PV: &energyforecast.PVConfig{LatitudeDeg: 57, LongitudeDeg: 15}, Load: &energyforecast.LoadConfig{}}}
	pv, load := 8000.0, 1000.0
	update, err := c.Update(context.Background(), energyforecast.UpdateRequest{RequestContext: meta, Observations: []energyforecast.Observation{{Interval: energyforecast.Interval{ValidStartMs: start.UnixMilli(), ValidEndMs: end.UnixMilli()}, Features: features(start), AvailableAtMs: end.UnixMilli(), PVAvailableW: &pv, HouseholdLoadW: &load, PVQuality: energyforecast.QualityGood, LoadQuality: energyforecast.QualityGood}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(update.State) == 0 || update.ModelRevision == 0 {
		t.Fatal("native update did not return versioned state")
	}
	meta.RequestID = "native-pv-predict"
	meta.State = update.State
	request := energyforecast.PredictRequest{RequestContext: meta, Horizon: []energyforecast.HorizonSlot{{Interval: energyforecast.Interval{ValidStartMs: end.UnixMilli(), ValidEndMs: end.Add(15 * time.Minute).UnixMilli()}, Features: features(end)}}}
	first, err := c.Predict(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Predict(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	encode := func(r energyforecast.PredictReply) string {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if encode(first) != encode(second) {
		t.Fatal("immutable native predict changed state")
	}
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, _ := newClient()
	replay, err := restarted.Predict(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if encode(first) != encode(replay) {
		t.Fatal("native state did not survive process restart")
	}
	for _, change := range []func(*energyforecast.PredictRequest){
		func(r *energyforecast.PredictRequest) { r.SiteID = "another-site" },
		func(r *energyforecast.PredictRequest) { r.ConfigRevision = "v2" },
		func(r *energyforecast.PredictRequest) { r.OriginMs = start.UnixMilli() },
	} {
		bad := request
		change(&bad)
		if _, err := restarted.Predict(context.Background(), bad); err == nil {
			t.Fatal("native worker accepted wrong site/config or future state")
		}
	}
}
