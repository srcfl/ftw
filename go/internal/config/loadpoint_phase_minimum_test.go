package config

import (
	"strings"
	"testing"
)

// srcfl/ftw#1861: a loadpoint pinned to 3p with min = max = 3,800 W cannot
// run three phases, which take at least 6 A each (4,140 W at 230 V). Startup
// and save say so, and say what FTW orders instead.
func TestLoadpointBelowPhaseMinimumWarns(t *testing.T) {
	for _, c := range []struct {
		name, mode string
		phases     int
		maxW       float64
		steps      []float64
		want       string
	}{
		{"3p, maximum below 6 A a phase", "3p", 3, 3800, []float64{0, 3800}, "below 4140 W"},
		{"3p, no table", "3p", 3, 3800, nil, "below 4140 W"},
		{"3p, a table with no three-phase level", "3p", 3, 5520, []float64{0, 1380, 2300, 3680}, "no level from 4140 W"},
		{"3p, legal", "3p", 3, 4140, []float64{0, 4140}, ""},
		{"unset runs on one phase", "", 3, 3800, []float64{0, 3800}, ""},
		{"auto runs on one phase", "auto", 3, 3800, nil, ""},
		{"one-phase site", "3p", 1, 3680, nil, ""},
		{"below 6 A on one phase", "1p", 3, 1000, nil, "below 1380 W"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{
				Drivers:    []Driver{{Name: "easee"}},
				Loadpoints: []Loadpoint{{ID: "rav4", DriverName: "easee", MinChargeW: min(c.maxW, 3800), MaxChargeW: c.maxW, PhaseMode: c.mode, AllowedStepsW: c.steps}},
			}
			cfg.Fuse.MaxAmps, cfg.Fuse.Phases, cfg.Fuse.Voltage = 20, c.phases, 230
			if err := cfg.normalizeLoadpointSteps(); err != nil {
				t.Fatal(err)
			}
			got := strings.Join(cfg.LoadpointWarnings, "\n")
			if c.want == "" && got != "" || c.want != "" && (!strings.Contains(got, c.want) || !strings.Contains(got, `"rav4"`)) {
				t.Fatalf("warnings %q, want one with %q", got, c.want)
			}
			// A fixed loadpoint drops the warning on the next save.
			cfg.Loadpoints[0].MaxChargeW, cfg.Loadpoints[0].AllowedStepsW = 11000, nil
			if err := cfg.normalizeLoadpointSteps(); err != nil {
				t.Fatal(err)
			}
			if len(cfg.LoadpointWarnings) != 0 {
				t.Fatalf("warnings after the fix: %q", cfg.LoadpointWarnings)
			}
		})
	}
}
