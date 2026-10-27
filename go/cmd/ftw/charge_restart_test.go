package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestRestartEndsManualCharge(t *testing.T) {
	for _, tc := range []struct {
		name         string
		planner      *config.Planner
		storedExport string
		want         control.Mode
	}{
		{"planner enabled", &config.Planner{Enabled: true}, "", control.ModePlannerPassiveArbitrage},
		{"export allowed", &config.Planner{Enabled: true}, "allowed", control.ModePlannerArbitrage},
		{"export refused", &config.Planner{Enabled: true, BatteryExport: "allowed"}, "not_allowed", control.ModePlannerPassiveArbitrage},
		{"export unknown", &config.Planner{Enabled: true, BatteryExport: "allowed"}, "unknown", control.ModePlannerPassiveArbitrage},
		{"export from config", &config.Planner{Enabled: true, BatteryExport: "allowed"}, "", control.ModePlannerArbitrage},
		{"invalid stored permission", &config.Planner{Enabled: true, BatteryExport: "allowed"}, "invalid", control.ModePlannerArbitrage},
		{"no export permission", &config.Planner{Enabled: true, BatteryExport: "invalid"}, "invalid", control.ModePlannerPassiveArbitrage},
		{"planner disabled", &config.Planner{Enabled: false}, "allowed", control.ModeIdle},
		{"no planner", nil, "", control.ModeIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := parseBatteryLimitConfig(t, "", "    max_charge_w: 10000\n    soc_max: 0.9\n")
			cfg.Planner = tc.planner
			path := filepath.Join(t.TempDir(), "state.db")
			st, err := state.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.SaveConfig(config.StateKeyBatteryExport, tc.storedExport); err != nil {
				t.Fatal(err)
			}
			// A new, explicit Charge still works during this process.
			ctrl := newControlStateFromConfig(cfg)
			modes := appModes{ctrl: ctrl, ctrlMu: &sync.Mutex{}, state: st}
			if err := modes.SetMode(context.Background(), control.ModeCharge); err != nil {
				t.Fatal(err)
			}
			if targets := control.ComputeDispatch(batteryLimitStore(1000), ctrl, map[string]float64{"battery": 15000}, 40000); len(targets) != 1 || targets[0].TargetW != 10000 {
				t.Fatalf("explicit Charge: %+v, want 10000 W", targets)
			}
			for boot := 1; boot <= 2; boot++ {
				if err := st.Close(); err != nil {
					t.Fatal(err)
				}
				st, err = state.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				ctrl = newControlStateFromConfig(cfg)
				restoreStoredMode(ctrl, st, cfg)
				if ctrl.Mode != tc.want {
					t.Fatalf("boot %d: mode = %q, want %q", boot, ctrl.Mode, tc.want)
				}
				if saved, _ := st.LoadConfig("mode"); saved != string(tc.want) {
					t.Fatalf("boot %d: stored mode = %q, want %q", boot, saved, tc.want)
				}
				if tc.name == "planner enabled" || tc.name == "export unknown" {
					if export, _ := st.LoadConfig(config.StateKeyBatteryExport); export != "unknown" {
						t.Fatalf("boot %d: unknown export permission became %q", boot, export)
					}
				}
				// Before the fresh plan arrives, import without solar must
				// not resume the earlier full-power charging order.
				for _, target := range control.ComputeDispatch(batteryLimitStore(1000), ctrl, map[string]float64{"battery": 15000}, 40000) {
					if target.TargetW > 0 {
						t.Fatalf("boot %d: charging before a fresh plan: %+v", boot, target)
					}
				}
			}
		})
	}
}
