package api

import (
	"github.com/srcfl/ftw/go/internal/telemetry"
	"testing"
	"time"
)

func TestIndependentControlConfirmation(t *testing.T) {
	now := time.Now()
	window := func(power float64, at time.Time) telemetry.ControlWindow {
		return telemetry.ControlWindow{MeanW: power, MinW: power - 10, MaxW: power + 10, First: at.Add(-6 * time.Second), Last: at, Count: 7}
	}
	base := map[string]telemetry.ControlBaseline{
		"battery:battery": {Driver: "battery", Kind: telemetry.DerBattery, Window: window(0, now.Add(-25*time.Second))},
		"grid:meter":      {Driver: "grid", Kind: telemetry.DerMeter, Window: window(400, now.Add(-25*time.Second))},
		"solar:pv":        {Driver: "solar", Kind: telemetry.DerPV, Window: window(-1200, now.Add(-25*time.Second))},
	}
	for _, tc := range []struct {
		name             string
		device, grid, pv float64
		separate         bool
		want             string
	}{
		{"one kW charge", 1000, 1400, -1200, true, "confirmed"},
		{"one kW discharge", -1000, -600, -1200, true, "confirmed"},
		{"noise within tolerance", 990, 1430, -1190, true, "confirmed"},
		{"new household load", 1000, 2600, -1200, true, "site_change_differs"},
		{"household load stopped", 1000, 200, -1200, true, "site_change_differs"},
		{"solar changed at the same time", 1000, 1400, -2200, true, "other_flows_changed"},
		{"same sensor twice", 1000, 1400, -1200, false, "independent_source_unknown"},
		{"too small to attribute", 100, 500, -1200, true, "no_clear_change"},
		{"wrong site direction", 1000, -600, -1200, true, "site_change_differs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := map[string]telemetry.ControlBaseline{
				"battery:battery": {Driver: "battery", Kind: telemetry.DerBattery, Window: window(tc.device, now)},
				"grid:meter":      {Driver: "grid", Kind: telemetry.DerMeter, Window: window(tc.grid, now)},
				"solar:pv":        {Driver: "solar", Kind: telemetry.DerPV, Window: window(tc.pv, now)},
			}
			c := telemetry.CommandEvidence{Driver: "battery", Kind: "battery", Since: now.Add(-20 * time.Second), Baseline: base}
			reason, _, _ := independentResponse(c, "grid", tc.separate, after, now)
			if reason != tc.want {
				t.Fatalf("got %s, want %s", reason, tc.want)
			}
			if tc.want == "confirmed" {
				for _, missing := range []string{"grid:meter", "battery:battery", "solar:pv"} {
					saved := after[missing]
					delete(after, missing)
					reason, _, _ = independentResponse(c, "grid", true, after, now)
					if reason == "confirmed" {
						t.Fatalf("confirmed without %s", missing)
					}
					after[missing] = saved
				}
				meter := after["grid:meter"]
				meter.Window.Last = now.Add(-time.Minute)
				after["grid:meter"] = meter
				reason, _, _ = independentResponse(c, "grid", true, after, now)
				if reason == "confirmed" {
					t.Fatal("stale meter confirmed effect")
				}
			}
		})
	}
}
