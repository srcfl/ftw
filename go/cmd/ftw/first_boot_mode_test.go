package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/state"
)

// bootBox opens a box's state the way main does on a start: it reads
// whether this is the first start, then records the start.
func bootBox(t *testing.T, st *state.Store) bool {
	t.Helper()
	first := isFirstBoot(st)
	if err := st.RecordEvent("startup"); err != nil {
		t.Fatal(err)
	}
	return first
}

func openState(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestSeedPlannerModeOnlyOnABrandNewBox(t *testing.T) {
	planner := &config.Config{Planner: &config.Planner{Enabled: true, BatteryExport: "not_allowed"}}
	for _, tc := range []struct {
		name    string
		earlier bool   // the box has started before
		stored  string // "" = no stored mode
		cfg     *config.Config
		export  config.BatteryExport
		want    control.Mode
		seeded  bool
	}{
		{"new box, planner on", false, "", planner, config.BatteryExportNotAllowed, control.ModePlannerPassiveArbitrage, true},
		// The household allowed selling in config, so the plan may sell.
		{"new box, planner on, selling allowed", false, "", planner, config.BatteryExportAllowed, control.ModePlannerArbitrage, true},
		{"new box, planner off", false, "", &config.Config{Planner: &config.Planner{Enabled: false}}, config.BatteryExportNotAllowed, control.ModeSelfConsumption, false},
		{"new box, no planner block", false, "", &config.Config{}, config.BatteryExportNotAllowed, control.ModeSelfConsumption, false},
		// An existing box that never picked a mode has run self_consumption
		// all along; an update must not hand it to the planner.
		{"existing box, planner on, no stored mode", true, "", planner, config.BatteryExportNotAllowed, control.ModeSelfConsumption, false},
		{"existing box, stored manual mode", true, "self_consumption", planner, config.BatteryExportNotAllowed, control.ModeSelfConsumption, false},
		{"existing box, stored idle", true, "idle", planner, config.BatteryExportNotAllowed, control.ModeIdle, false},
		{"existing box, stored planner mode", true, "planner_passive_arbitrage", planner, config.BatteryExportAllowed, control.ModePlannerPassiveArbitrage, false},
		// A stored mode wins even on a first start (a restored settings file).
		{"new box, stored mode", false, "self_consumption", planner, config.BatteryExportNotAllowed, control.ModeSelfConsumption, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openState(t)
			if tc.earlier {
				bootBox(t, st)
			}
			if tc.stored != "" {
				if err := st.SaveConfig("mode", tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			first := bootBox(t, st)
			if first == tc.earlier {
				t.Fatalf("first boot = %v for a box that started before = %v", first, tc.earlier)
			}
			ctrl := control.NewState(0, 50, "meter")
			restoreStoredMode(ctrl, st, tc.cfg)

			if got := seedPlannerMode(ctrl, st, tc.cfg, tc.export, first); got != tc.seeded {
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

// The second start finds its own startup record and the stored mode, and
// keeps whatever the household picked since.
func TestSeedPlannerModeOnlyOnce(t *testing.T) {
	cfg := &config.Config{Planner: &config.Planner{Enabled: true}}
	st := openState(t)
	if !seedPlannerMode(control.NewState(0, 50, "meter"), st, cfg, config.BatteryExportNotAllowed, bootBox(t, st)) {
		t.Fatal("first start did not seed")
	}
	if err := st.SaveConfig("mode", string(control.ModeSelfConsumption)); err != nil {
		t.Fatal(err)
	}
	ctrl := control.NewState(0, 50, "meter")
	restoreStoredMode(ctrl, st, cfg)
	if seedPlannerMode(ctrl, st, cfg, config.BatteryExportNotAllowed, bootBox(t, st)) || ctrl.Mode != control.ModeSelfConsumption {
		t.Fatalf("second start changed the household's mode to %q", ctrl.Mode)
	}
}

// Settings the setup wizard stores before the first start are not a start.
func TestFirstBootAfterTheWizardStoredSettings(t *testing.T) {
	st := openState(t)
	if err := st.SaveConfig("forecast_trust", "balanced"); err != nil {
		t.Fatal(err)
	}
	if !isFirstBoot(st) {
		t.Fatal("stored settings made a new box look started")
	}
	if isFirstBoot(nil) {
		t.Fatal("no state counts as a first start")
	}
}

// The guide's own path: the wizard saves its settings and the house password
// into a new state.db before the first start. That box still counts as new,
// so it starts in the planner mode.
func TestWizardBoxStartsInThePlannerMode(t *testing.T) {
	t.Setenv("FTW_API_TOKEN", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	restarted := make(chan struct{}, 1)
	handler := secureBootstrapMutations(bootstrapConfig(path, func(cfg *config.Config) error { return saveInitialConfig(path, cfg) },
		func() error { restarted <- struct{}{}; return nil }))
	body := `{"site":{"name":"Home","control_interval_s":5,"grid_tolerance_w":42,"watchdog_timeout_s":60,"smoothing_alpha":0.3,"gain":0.5,"slew_rate_w":500,"min_dispatch_interval_s":5},` +
		`"fuse":{"max_amps":16,"phases":3,"voltage":230},"drivers":[],"api":{"port":8080},` +
		`"planner":{"enabled":true,"battery_export":"not_allowed"},"house_password":"test-house-password"}`
	req := httptest.NewRequest("POST", "http://box.local/api/config", strings.NewReader(body))
	req.RemoteAddr = "192.168.1.12:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://box.local")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	<-restarted
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConfigDatabase == "" {
		t.Fatal("the wizard stored no settings database; this test no longer covers that path")
	}
	st := mustOpenState(t, cfg.ConfigDatabase)
	first := bootBox(t, st)
	if !first {
		t.Fatal("the wizard's settings made the new box look started")
	}
	_, export, _, _ := config.ResolvePlannerPrefs("", "", "", string(control.ModeSelfConsumption), "", cfg.Planner.BatteryExport, nil)
	ctrl := control.NewState(0, 50, "meter")
	restoreStoredMode(ctrl, st, cfg)
	if !seedPlannerMode(ctrl, st, cfg, export, first) || ctrl.Mode != control.ModePlannerPassiveArbitrage {
		t.Fatalf("mode = %q, want the passive planner on the first start", ctrl.Mode)
	}
}
