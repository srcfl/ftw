package api

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/telemetry"
)

func responseSeries(driver string, kind telemetry.DerType, at time.Time, values []float64) telemetry.ControlBaseline {
	r := telemetry.ControlBaseline{Driver: driver, Kind: kind}
	for i, v := range values {
		r.Points = append(r.Points, telemetry.ControlObservation{PowerW: v, At: at.Add(time.Duration(i) * 3 * time.Second)})
	}
	r.Window = telemetry.ControlWindow{MeanW: values[0], MinW: values[0], MaxW: values[0], First: r.Points[0].At, Last: r.Points[len(r.Points)-1].At, Count: len(values)}
	return r
}

func TestIndependentControlConfirmation(t *testing.T) {
	now := time.Now()
	flat := func(v float64) []float64 { return []float64{v, v, v, v, v} }
	for _, tc := range []struct {
		name             string
		device, grid, pv []float64
		separate         bool
		want             string
	}{
		{"one kW charge", flat(1000), flat(1400), flat(-1200), true, "confirmed"},
		{"one kW discharge", flat(-1000), flat(-600), flat(-1200), true, "confirmed"},
		{"noise within tolerance", flat(990), []float64{1420, 1380, 1440, 1400, 1410}, flat(-1200), true, "confirmed"},
		{"solar offsets charging at the meter", flat(1000), flat(400), flat(-2200), true, "confirmed"},
		{"cloud moves while charging ramps", []float64{400, 700, 1000, 1000, 1000}, []float64{600, 200, 100, 600, 1100}, []float64{-1400, -2100, -2500, -2000, -1500}, true, "confirmed"},
		{"household load starts", flat(1000), flat(2600), flat(-1200), true, "site_change_differs"},
		{"household load stops", flat(1000), flat(200), flat(-1200), true, "site_change_differs"},
		{"pulse disappears in the mean", flat(1000), []float64{1400, 2400, 1400, 400, 1400}, flat(-1200), true, "site_change_differs"},
		{"reported solar change absent from grid", flat(1000), flat(1400), flat(-2200), true, "site_change_differs"},
		{"same physical sensor", flat(1000), flat(1400), flat(-1200), false, "independent_source_unknown"},
		{"small change", flat(100), flat(500), flat(-1200), true, "no_clear_change"},
		{"impossible balance", flat(5000), flat(0), flat(-1200), true, "energy_balance_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := map[string]telemetry.ControlBaseline{
				"battery:battery": responseSeries("battery", telemetry.DerBattery, now.Add(-30*time.Second), flat(0)),
				"grid:meter":      responseSeries("grid", telemetry.DerMeter, now.Add(-30*time.Second), flat(400)),
				"solar:pv":        responseSeries("solar", telemetry.DerPV, now.Add(-30*time.Second), flat(-1200)),
			}
			after := map[string]telemetry.ControlBaseline{
				"battery:battery": responseSeries("battery", telemetry.DerBattery, now.Add(-12*time.Second), tc.device),
				"grid:meter":      responseSeries("grid", telemetry.DerMeter, now.Add(-12*time.Second), tc.grid),
				"solar:pv":        responseSeries("solar", telemetry.DerPV, now.Add(-12*time.Second), tc.pv),
			}
			cmd := telemetry.CommandEvidence{Driver: "battery", Kind: "battery", Since: now.Add(-15 * time.Second), Baseline: base}
			got := independentResponse(cmd, "grid", tc.separate, after, now)
			if got.Reason != tc.want {
				t.Fatalf("got %+v, want %s", got, tc.want)
			}
			if tc.want != "confirmed" {
				return
			}
			if got.Samples != 5 || got.WindowS != 12 || len(got.Trace) != 5 {
				t.Fatalf("missing trace: %+v", got)
			}
			for _, missing := range []string{"grid:meter", "battery:battery", "solar:pv"} {
				saved := after[missing]
				delete(after, missing)
				if independentResponse(cmd, "grid", true, after, now).Reason == "confirmed" {
					t.Fatalf("confirmed without %s", missing)
				}
				after[missing] = saved
			}
			// A delayed or reused source cannot manufacture aligned samples.
			saved := after["solar:pv"]
			delayed := saved
			delayed.Points = append([]telemetry.ControlObservation(nil), saved.Points...)
			for i := range delayed.Points {
				delayed.Points[i].At = now.Add(-30 * time.Second)
			}
			after["solar:pv"] = delayed
			if independentResponse(cmd, "grid", true, after, now).Reason == "confirmed" {
				t.Fatal("cached solar confirmed")
			}
			after["solar:pv"] = saved
			if independentResponse(cmd, "grid", true, after, now.Add(time.Minute)).Reason == "confirmed" {
				t.Fatal("stale site confirmed")
			}
		})
	}
}

func TestIndependentResponseReplaysChangingSites(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	now := time.Now()
	for run := 0; run < 250; run++ {
		direction := 1.0
		if run%2 == 1 {
			direction = -1
		}
		step := direction * (600 + rng.Float64()*4400)
		house := 500 + rng.Float64()*4000
		baseSolar := -500 - rng.Float64()*6000
		beforeGrid, beforePV, zero, device, grid, pv := []float64{}, []float64{}, []float64{}, []float64{}, []float64{}, []float64{}
		for i := 0; i < 5; i++ {
			solar := baseSolar + rng.Float64()*400
			beforePV = append(beforePV, solar)
			beforeGrid = append(beforeGrid, house+solar)
			zero = append(zero, 0)
			solar = baseSolar - rng.Float64()*1500
			power := step * math.Min(1, float64(i+1)/3)
			device = append(device, power)
			pv = append(pv, solar)
			grid = append(grid, house+solar+power+(rng.Float64()-.5)*40)
		}
		cmd := telemetry.CommandEvidence{Driver: "battery", Kind: "battery", Since: now.Add(-15 * time.Second), Baseline: map[string]telemetry.ControlBaseline{
			"battery:battery": responseSeries("battery", telemetry.DerBattery, now.Add(-30*time.Second), zero),
			"grid:meter":      responseSeries("grid", telemetry.DerMeter, now.Add(-30*time.Second), beforeGrid),
			"solar:pv":        responseSeries("solar", telemetry.DerPV, now.Add(-30*time.Second), beforePV),
		}}
		after := map[string]telemetry.ControlBaseline{
			"battery:battery": responseSeries("battery", telemetry.DerBattery, now.Add(-12*time.Second), device),
			"grid:meter":      responseSeries("grid", telemetry.DerMeter, now.Add(-12*time.Second), grid),
			"solar:pv":        responseSeries("solar", telemetry.DerPV, now.Add(-12*time.Second), pv),
		}
		if got := independentResponse(cmd, "grid", true, after, now); got.Reason != "confirmed" {
			t.Fatalf("replay %d: %+v", run, got)
		}
		grid[2] += 2000 // A load pulse cannot disappear into an average or solar correction.
		after["grid:meter"] = responseSeries("grid", telemetry.DerMeter, now.Add(-12*time.Second), grid)
		if got := independentResponse(cmd, "grid", true, after, now); got.Reason == "confirmed" {
			t.Fatalf("replay %d accepted an unexplained pulse", run)
		}
	}
}

func TestControlConfirmationRequiresConfiguredFlows(t *testing.T) {
	now := time.Now()
	tel := telemetry.NewStore()
	tel.Update("battery", telemetry.DerBattery, 1000, nil, []byte(`{}`))
	tel.RecordDriverSuccess("battery")
	observed := map[string]telemetry.ControlBaseline{"battery:battery": responseSeries("battery", telemetry.DerBattery, now.Add(-12*time.Second), []float64{1000, 1000, 1000, 1000, 1000})}
	srv := New(&Deps{Tel: tel})
	if srv.controlSourcesComplete(observed, now) {
		t.Fatal("missing site inventory claimed complete")
	}
	srv.deps.SiteMeasurementSources = func() telemetry.ForecastOptions {
		return telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}, {Driver: "solar", DerType: telemetry.DerPV}}}
	}
	if srv.controlSourcesComplete(observed, now) {
		t.Fatal("configured solar that never emitted disappeared from proof")
	}
	srv.deps.SiteMeasurementSources = func() telemetry.ForecastOptions {
		return telemetry.ForecastOptions{ExpectedFlows: []telemetry.ForecastFlow{{Driver: "battery", DerType: telemetry.DerBattery}}}
	}
	if !srv.controlSourcesComplete(observed, now) {
		t.Fatal("complete source inventory rejected")
	}
}
