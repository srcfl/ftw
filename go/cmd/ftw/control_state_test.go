package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/srcfl/ftw/go/internal/config"
	"github.com/srcfl/ftw/go/internal/control"
	"github.com/srcfl/ftw/go/internal/mpc"
	"github.com/srcfl/ftw/go/internal/state"
	"github.com/srcfl/ftw/go/internal/telemetry"
)

func TestControlStateFromConfigAppliesSiteGain(t *testing.T) {
	cfg := &config.Config{
		Site: config.Site{
			Gain:                 0.8,
			GridToleranceW:       50,
			SlewRateW:            500,
			MinDispatchIntervalS: 5,
		},
	}
	ctrl := newControlStateFromConfig(cfg)
	if ctrl.PI.Kp != 0.8 {
		t.Fatalf("PI.Kp = %f, want configured site.gain", ctrl.PI.Kp)
	}
}

func TestControlSlotDirectiveFromMPCPreservesDecisionIdentity(t *testing.T) {
	start := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	loadpoints := map[string]float64{"ev": 250}
	in := mpc.SlotDirective{
		DecisionID: "00000000-0000-4000-8000-000000000123",
		SlotStart:  start, SlotEnd: start.Add(15 * time.Minute),
		BatteryEnergyWh: 100, SoCTarget: 0.42, Strategy: mpc.ModePassiveArbitrage,
		PVLimitW: 7000, GridW: -6400, LivePVSurplusSoCCap: 0.65,
		LoadpointEnergyWh: loadpoints,
	}

	got := control.SlotDirectiveFromMPC(in)
	if got.DecisionID != in.DecisionID || !got.SlotStart.Equal(in.SlotStart) || !got.SlotEnd.Equal(in.SlotEnd) {
		t.Fatalf("identity or timing changed across adapter: %+v", got)
	}
	if got.BatteryEnergyWh != in.BatteryEnergyWh || got.PlannedGridW != in.GridW || !got.HasPlannedGridW {
		t.Fatalf("power allocation changed across adapter: %+v", got)
	}
	if !reflect.DeepEqual(got.LoadpointEnergyWh, loadpoints) {
		t.Fatalf("loadpoint allocation changed across adapter: %+v", got.LoadpointEnergyWh)
	}
}

func TestControlSlotDirectiveFromMPCPreservesEVOnPower(t *testing.T) {
	start := time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)
	in := mpc.SlotDirective{
		SlotStart: start, SlotEnd: start.Add(15 * time.Minute),
		LoadpointEnergyWh:  map[string]float64{"easee": 100},
		LoadpointMaxPowerW: map[string]float64{"easee": 10350},
		Strategy:           mpc.ModeArbitrage,
	}
	got := control.SlotDirectiveFromMPC(in)
	if !reflect.DeepEqual(got.LoadpointMaxPowerW, in.LoadpointMaxPowerW) {
		t.Fatalf("EV on-power dropped across adapter: %+v", got.LoadpointMaxPowerW)
	}
	if !reflect.DeepEqual(got.LoadpointEnergyWh, in.LoadpointEnergyWh) {
		t.Fatalf("EV energy budget dropped across adapter: %+v", got.LoadpointEnergyWh)
	}
}

func parseBatteryLimitConfig(t *testing.T, driverLimits, batteryLimits string) *config.Config {
	t.Helper()
	yaml := `
site:
  name: Limit test
fuse:
  max_amps: 63
  phases: 3
  voltage: 230
api:
  port: 8080
drivers:
  - name: battery
    lua: battery.lua
    is_site_meter: true
    battery_capacity_wh: 10000
    capabilities:
      standalone: true
` + driverLimits + `
batteries:
  battery:
` + batteryLimits
	cfg, err := config.Parse([]byte(yaml), t.TempDir())
	if err != nil {
		t.Fatalf("parse battery limit config: %v", err)
	}
	return cfg
}

func batteryLimitStore(gridW float64) *telemetry.Store {
	store := telemetry.NewStore()
	store.Update("battery", telemetry.DerMeter, gridW, nil, nil)
	soc := 0.5
	store.Update("battery", telemetry.DerBattery, 0, &soc, nil)
	store.DriverHealthMut("battery").RecordSuccess()
	return store
}

func TestBatteryLimitConfigExplicitZeroChargeReachesControl(t *testing.T) {
	cfg := parseBatteryLimitConfig(t,
		"    max_charge_w: 7000\n    max_discharge_w: 6000\n",
		"    max_charge_w: 0\n    max_discharge_w: 4000\n")
	ctrl := newControlStateFromConfig(cfg)
	lim := ctrl.DriverLimits["battery"]
	if !lim.MaxChargeWSet || lim.MaxChargeW != 0 {
		t.Fatalf("charge limit lost config presence: %+v", lim)
	}
	ctrl.Mode = control.ModeCharge
	targets := control.ComputeDispatch(batteryLimitStore(-6000), ctrl, map[string]float64{"battery": 10000}, 40000)
	if len(targets) != 1 || targets[0].TargetW != 0 {
		t.Fatalf("batteries.battery.max_charge_w=0 produced %+v, want one 0 W target", targets)
	}
}

func TestBatteryLimitConfigExplicitZeroDischargeReachesControl(t *testing.T) {
	cfg := parseBatteryLimitConfig(t,
		"    max_charge_w: 7000\n    max_discharge_w: 6000\n",
		"    max_charge_w: 4000\n    max_discharge_w: 0\n")
	ctrl := newControlStateFromConfig(cfg)
	lim := ctrl.DriverLimits["battery"]
	if !lim.MaxDischargeWSet || lim.MaxDischargeW != 0 {
		t.Fatalf("discharge limit lost config presence: %+v", lim)
	}
	ctrl.Mode = control.ModeSelfConsumption
	ctrl.SlewRateW = 100000
	ctrl.MinDispatchIntervalS = 0
	targets := control.ComputeDispatch(batteryLimitStore(12000), ctrl, map[string]float64{"battery": 10000}, 40000)
	if len(targets) != 1 || targets[0].TargetW != 0 {
		t.Fatalf("batteries.battery.max_discharge_w=0 produced %+v, want one 0 W target", targets)
	}
}

func TestBatteryLimitConfigUnsetUsesDriverValue(t *testing.T) {
	cfg := parseBatteryLimitConfig(t,
		"    max_charge_w: 7000\n    max_discharge_w: 6000\n",
		"    weight: 1\n")
	ctrl := newControlStateFromConfig(cfg)
	ctrl.Mode = control.ModeCharge
	targets := control.ComputeDispatch(batteryLimitStore(0), ctrl, map[string]float64{"battery": 10000}, 40000)
	if len(targets) != 1 || targets[0].TargetW != 7000 {
		t.Fatalf("unset battery charge limit produced %+v, want configured driver limit 7000 W", targets)
	}
}

func TestBatteryLimitConfigBothZeroUsesDefault(t *testing.T) {
	cfg := parseBatteryLimitConfig(t, "",
		"    max_charge_w: 0\n    max_discharge_w: 0\n")
	ctrl := newControlStateFromConfig(cfg)
	lim, ok := ctrl.DriverLimits["battery"]
	if !ok || !lim.MaxChargeWSet || !lim.MaxDischargeWSet || lim.MaxChargeW != 5000 || lim.MaxDischargeW != 5000 {
		t.Fatalf("both-zero config error did not share the MPC default: %+v ok=%v", lim, ok)
	}
	ctrl.Mode = control.ModeCharge
	targets := control.ComputeDispatch(batteryLimitStore(0), ctrl, map[string]float64{"battery": 10000}, 40000)
	if len(targets) != 1 || targets[0].TargetW != 5000 {
		t.Fatalf("both-zero config error produced %+v, want the default 5000 W", targets)
	}
}

// A "NaN" saved from an MQTT command must not come back as the PI setpoint
// on every boot.
func TestRestoredGridTargetIgnoresNonFinite(t *testing.T) {
	for _, stored := range []string{"NaN", "nan", "+Inf", "-Inf", "not-a-number"} {
		if f, ok := restoredGridTargetW(stored); ok {
			t.Errorf("restoredGridTargetW(%q) = %v, true; want ignored", stored, f)
		}
	}
	if f, ok := restoredGridTargetW("-1500.0"); !ok || f != -1500 {
		t.Fatalf("restoredGridTargetW(-1500.0) = %v, %v; want -1500, true", f, ok)
	}
}

// Priority and weighted were removed. A site that stored one must boot into
// self-consumption and store it, not keep a mode no door accepts any more.
func TestRestoreStoredModeMapsRemovedModesToSelfConsumption(t *testing.T) {
	for stored, want := range map[string]control.Mode{
		"priority":          control.ModeSelfConsumption,
		"weighted":          control.ModeSelfConsumption,
		"planner_arbitrage": control.ModePlannerArbitrage,
		"peak_shaving":      control.ModePeakShaving,
	} {
		t.Run(stored, func(t *testing.T) {
			st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.SaveConfig("mode", stored); err != nil {
				t.Fatal(err)
			}
			ctrl := control.NewState(0, 50, "meter")
			ctrl.Mode = control.ModeIdle

			restoreStoredMode(ctrl, st)

			if ctrl.Mode != want {
				t.Fatalf("mode = %q, want %q", ctrl.Mode, want)
			}
			if saved, _ := st.LoadConfig("mode"); saved != string(want) {
				t.Fatalf("stored mode = %q, want %q", saved, want)
			}
		})
	}
}

// A value FTW never had keeps the default and is left for an operator to see.
func TestRestoreStoredModeLeavesAnUnknownValueAlone(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveConfig("mode", "turbo"); err != nil {
		t.Fatal(err)
	}
	ctrl := control.NewState(0, 50, "meter")

	restoreStoredMode(ctrl, st)

	if ctrl.Mode != control.ModeSelfConsumption {
		t.Fatalf("mode = %q, want the default", ctrl.Mode)
	}
	if saved, _ := st.LoadConfig("mode"); saved != "turbo" {
		t.Fatalf("stored mode = %q, want it untouched", saved)
	}
}

// Boot and hot reload read the dispatch path from one place. A reload that
// removed the planner section used to keep the legacy path a fresh boot
// would not pick.
func TestEnergyDispatchEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		planner *config.Planner
		want    bool
	}{
		{"no planner section", nil, true},
		{"planner without legacy_dispatch", &config.Planner{Enabled: true}, true},
		{"legacy_dispatch", &config.Planner{Enabled: true, LegacyDispatch: true}, false},
	} {
		if got := energyDispatchEnabled(&config.Config{Planner: tc.planner}); got != tc.want {
			t.Errorf("%s: energyDispatchEnabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}
