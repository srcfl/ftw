package main

import (
	"path/filepath"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

func TestSeedPlannerModeOnANewBox(t *testing.T) {
	planner := &config.Config{Planner: &config.Planner{Enabled: true, BatteryExport: "not_allowed"}}
	for _, tc := range []struct {
		name   string
		stored string // "" = no stored mode, a new box
		cfg    *config.Config
		export config.BatteryExport
		want   control.Mode
		seeded bool
	}{
		{"new box, planner on", "", planner, config.BatteryExportNotAllowed, control.ModePlannerPassiveArbitrage, true},
		// The household allowed selling in config, so the plan may sell.
		{"new box, planner on, selling allowed", "", planner, config.BatteryExportAllowed, control.ModePlannerArbitrage, true},
		{"new box, planner off", "", &config.Config{Planner: &config.Planner{Enabled: false}}, config.BatteryExportNotAllowed, control.ModeSelfConsumption, false},
		{"new box, no planner block", "", &config.Config{}, config.BatteryExportNotAllowed, control.ModeSelfConsumption, false},
		{"stored manual mode stays", "self_consumption", planner, config.BatteryExportNotAllowed, control.ModeSelfConsumption, false},
		{"stored idle stays", "idle", planner, config.BatteryExportNotAllowed, control.ModeIdle, false},
		{"stored planner mode stays", "planner_passive_arbitrage", planner, config.BatteryExportAllowed, control.ModePlannerPassiveArbitrage, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if tc.stored != "" {
				if err := st.SaveConfig("mode", tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			ctrl := control.NewState(0, 50, "meter")
			restoreStoredMode(ctrl, st, tc.cfg)

			if got := seedPlannerMode(ctrl, st, tc.cfg, tc.export); got != tc.seeded {
				t.Fatalf("seeded = %v, want %v", got, tc.seeded)
			}
			if ctrl.Mode != tc.want {
				t.Fatalf("mode = %q, want %q", ctrl.Mode, tc.want)
			}
			saved, ok := st.LoadConfig("mode")
			switch {
			case tc.seeded && saved != string(tc.want):
				t.Fatalf("stored mode = %q, want %q", saved, tc.want)
			case !tc.seeded && tc.stored == "" && ok:
				t.Fatalf("stored mode %q on a box that was not seeded", saved)
			case !tc.seeded && tc.stored != "" && saved != tc.stored:
				t.Fatalf("stored mode changed from %q to %q", tc.stored, saved)
			}
		})
	}
}

// The seed happens once: the second boot finds the stored mode and keeps
// whatever the household picked since.
func TestSeedPlannerModeOnlyOnce(t *testing.T) {
	cfg := &config.Config{Planner: &config.Planner{Enabled: true}}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if !seedPlannerMode(control.NewState(0, 50, "meter"), st, cfg, config.BatteryExportNotAllowed) {
		t.Fatal("first boot did not seed")
	}
	if err := st.SaveConfig("mode", string(control.ModeSelfConsumption)); err != nil {
		t.Fatal(err)
	}
	ctrl := control.NewState(0, 50, "meter")
	restoreStoredMode(ctrl, st, cfg)
	if seedPlannerMode(ctrl, st, cfg, config.BatteryExportNotAllowed) || ctrl.Mode != control.ModeSelfConsumption {
		t.Fatalf("second boot changed the household's mode to %q", ctrl.Mode)
	}
}
